// Package server exposes a mountable HTTP surface for a tinyTiles Dataset.
// It deliberately owns no listener, TLS configuration, authentication, or
// deployment policy. Use Handler with an existing application mux, or use the
// cmd/tinytiles-server binary for the small standalone application.
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tinytiles "github.com/Karte-Bayern/tinyTiles/v2"
	"github.com/Karte-Bayern/tinyTiles/v2/offline"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

var (
	errXYZPath       = errors.New("need z/x/y")
	errXYZCoordinate = errors.New("invalid coordinate")
	// Header.Set canonicalizes names on every call. The request hot paths use
	// these pre-canonicalized keys with direct replacement below instead.
	headerETag                = textproto.CanonicalMIMEHeaderKey("ETag")
	headerTileChecksum        = textproto.CanonicalMIMEHeaderKey(offline.HeaderTileChecksum)
	headerTileContentEncoding = textproto.CanonicalMIMEHeaderKey(offline.HeaderTileContentEncoding)
)

// Config controls the HTTP representation of a Dataset. Dataset remains owned
// by the caller and must be closed after its HTTP server has shut down.
type Config struct {
	Dataset *tinytiles.Dataset

	// DatasetID is the stable name used by the offline synchronization
	// manifest. It is not derived from a filesystem path.
	DatasetID string
	// PublicBase is an optional canonical http(s) origin or base path used in
	// TileJSON and sync manifests. Without it, the server derives an origin
	// from the incoming request; deployments behind TLS proxies should set it.
	// PublicBase takes precedence over MountPath and may itself include the
	// complete public path prefix.
	PublicBase string
	// MountPath is the optional absolute path below which Handler is mounted,
	// for example "/tinytiles". It does not mount or strip the handler itself;
	// use http.StripPrefix at the application boundary. When PublicBase is
	// empty, dynamically generated TileJSON and sync manifests append this path
	// to the incoming request origin so their URLs remain reachable through the
	// outer mount. Empty and "/" mean the root path.
	MountPath string
	// CORSOrigin is empty for no CORS header, * for public access, or one
	// http(s) origin.
	CORSOrigin string
	// ContentType, TileExtension and ContentEncoding override MBTiles metadata.
	// Empty ContentType and TileExtension infer vector/raster HTTP semantics
	// from the standard format field; empty ContentEncoding infers gzip from
	// kb:content_encoding.
	ContentType     string
	TileExtension   string
	ContentEncoding string
	// DEMEncoding declares a raster tileset as elevation data and is published
	// in TileJSON as "encoding": one of "terrarium", "mapbox" (Terrain-RGB) or
	// "custom". Empty infers it from the MBTiles format name or an "encoding"
	// metadata row. Terrain sources commonly record only format=png, so this
	// is how an existing DEM tileset is declared without rebuilding it. It is
	// ignored for a vector tileset.
	DEMEncoding string
	// TileCacheBytes bounds the immutable in-process tile cache. In addition to
	// hot payloads it retains compact SHA-256 values after payload eviction, so
	// a repeated uncached read avoids rehashing the full tile. Zero selects
	// DefaultTileCacheBytes; set -1 to disable it for strictly minimal memory
	// deployments.
	TileCacheBytes int64
	// PrefetchWorkers is the number of bounded background route-prediction
	// workers. Zero selects DefaultPrefetchWorkers; set -1 to disable
	// predictive caching entirely.
	PrefetchWorkers int
	// PrefetchQueue is the number of predicted tile keys accepted before new
	// predictions are dropped. Zero selects DefaultPrefetchQueue.
	PrefetchQueue int
	// PrefetchMaxTiles bounds work accepted from one route. Zero selects
	// DefaultPrefetchMaxTiles.
	PrefetchMaxTiles int
}

// Server is a handler factory. It is safe for concurrent requests as long as
// its current Dataset remains open. Serving state is held in an immutable
// generation, swapped atomically by SwapDataset so a running Server can move
// to a newly published artifact revision without dropping in-flight requests.
type Server struct {
	datasetID               string
	publicBase              string
	mountPath               string
	corsOrigin              string
	tileCacheBytes          int64
	contentTypeOverride     string
	tileExtensionOverride   string
	contentEncodingOverride string
	demEncodingOverride     string
	prefetchWorkers         int
	prefetchQueue           int
	prefetchMaxTiles        int
	prefetchMu              sync.Mutex
	prefetcher              *tilePrefetcher
	prefetchClosed          bool

	gen atomic.Pointer[generation]
}

// generation is every piece of serving state derived from one Dataset. It is
// never mutated after buildGeneration publishes it, so concurrent requests
// can read a loaded *generation without locking.
type generation struct {
	dataset         *tinytiles.Dataset
	revision        string
	createdAt       time.Time
	metadata        map[string]string
	contentType     string
	tileExtension   string
	tilePathSuffix  string
	contentEncoding string
	demEncoding     string
	tileCache       *tileCache
	tileLoads       tileLoadGroup
	metadataPayload []byte
	metadataETag    string
	manifestPayload []byte
	tileJSONPayload []byte
	tileJSONETag    string

	// The *Gzip fields hold a precomputed gzip encoding of the corresponding
	// payload above, or nil when compressing did not actually shrink it (a
	// tiny metadata map, for example). Computing this once per generation
	// instead of per request means a gzip-capable client gets fewer bytes on
	// the wire at zero additional request-path CPU cost.
	metadataPayloadGzip []byte
	manifestPayloadGzip []byte
	tileJSONPayloadGzip []byte
}

// New validates the static server configuration. It does not open or close
// files: Dataset.Open has already fail-closed validated the artifact.
func New(config Config) (*Server, error) {
	if config.Dataset == nil {
		return nil, errors.New("tinytiles server: dataset is required")
	}
	if err := validateDatasetID(config.DatasetID); err != nil {
		return nil, err
	}
	publicBase, err := normalizePublicBase(config.PublicBase)
	if err != nil {
		return nil, err
	}
	mountPath, err := normalizeMountPath(config.MountPath)
	if err != nil {
		return nil, err
	}
	corsOrigin, err := normalizeCORSOrigin(config.CORSOrigin)
	if err != nil {
		return nil, err
	}
	if config.TileCacheBytes < -1 {
		return nil, errors.New("tinytiles server: tile cache bytes must be non-negative or -1 to disable")
	}
	if config.PrefetchWorkers < -1 || config.PrefetchQueue < 0 || config.PrefetchMaxTiles < 0 {
		return nil, errors.New("tinytiles server: invalid predictive cache configuration")
	}
	tileCacheBytes := config.TileCacheBytes
	if tileCacheBytes == 0 {
		tileCacheBytes = DefaultTileCacheBytes
	}
	prefetchWorkers := config.PrefetchWorkers
	if prefetchWorkers == 0 {
		prefetchWorkers = DefaultPrefetchWorkers
	}
	prefetchQueue := config.PrefetchQueue
	if prefetchQueue == 0 {
		prefetchQueue = DefaultPrefetchQueue
	}
	prefetchMaxTiles := config.PrefetchMaxTiles
	if prefetchMaxTiles == 0 {
		prefetchMaxTiles = DefaultPrefetchMaxTiles
	}
	server := &Server{
		datasetID:               config.DatasetID,
		publicBase:              publicBase,
		mountPath:               mountPath,
		corsOrigin:              corsOrigin,
		tileCacheBytes:          tileCacheBytes,
		contentTypeOverride:     config.ContentType,
		tileExtensionOverride:   config.TileExtension,
		contentEncodingOverride: config.ContentEncoding,
		demEncodingOverride:     config.DEMEncoding,
		prefetchWorkers:         prefetchWorkers,
		prefetchQueue:           prefetchQueue,
		prefetchMaxTiles:        prefetchMaxTiles,
	}
	gen, err := server.buildGeneration(config.Dataset)
	if err != nil {
		return nil, err
	}
	server.gen.Store(gen)
	return server, nil
}

// SwapDataset atomically moves this Server from its current Dataset to
// newDataset. In-flight requests started before the swap keep running against
// the generation they already loaded; every request accepted after SwapDataset
// returns serves newDataset. It returns the Dataset the Server served before
// the swap so the caller can close it — Dataset.Close already waits for its
// own in-flight lookups, so calling it immediately after a successful swap is
// safe and does not need an additional drain delay.
//
// A rejected newDataset (for example, one that fails the same validation New
// performs) leaves the Server serving its previous generation unchanged; the
// caller keeps ownership of newDataset and should close it.
func (s *Server) SwapDataset(newDataset *tinytiles.Dataset) (*tinytiles.Dataset, error) {
	if s == nil {
		return nil, errors.New("tinytiles server: server is nil")
	}
	if newDataset == nil {
		return nil, errors.New("tinytiles server: new dataset is required")
	}
	gen, err := s.buildGeneration(newDataset)
	if err != nil {
		return nil, err
	}
	previous := s.gen.Swap(gen)
	if previous == nil {
		return nil, nil
	}
	return previous.dataset, nil
}

// buildGeneration performs the same artifact/metadata validation and payload
// precomputation as New, against whichever Dataset is being made current.
func (s *Server) buildGeneration(dataset *tinytiles.Dataset) (*generation, error) {
	info := dataset.Info()
	if len(info.TileDigestSHA256) != sha256.Size*2 {
		return nil, errors.New("tinytiles server: artifact has no usable tile digest revision")
	}
	if _, err := hex.DecodeString(info.TileDigestSHA256); err != nil {
		return nil, fmt.Errorf("tinytiles server: invalid tile digest revision: %w", err)
	}
	metadata, err := dataset.Metadata()
	if err != nil {
		return nil, fmt.Errorf("tinytiles server: read metadata: %w", err)
	}
	format, err := inferTileFormat(metadata, s.contentTypeOverride, s.tileExtensionOverride, s.demEncodingOverride)
	if err != nil {
		return nil, err
	}
	contentType := format.contentType
	contentEncoding := strings.TrimSpace(s.contentEncodingOverride)
	if contentEncoding == "" && strings.EqualFold(metadata["kb:content_encoding"], "gzip") {
		contentEncoding = "gzip"
	}
	metadataPayload, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("tinytiles server: encode metadata: %w", err)
	}
	gen := &generation{
		dataset:             dataset,
		revision:            info.TileDigestSHA256,
		createdAt:           info.CreatedAt,
		metadata:            metadata,
		contentType:         contentType,
		tileExtension:       format.extension,
		tilePathSuffix:      "." + format.extension,
		contentEncoding:     contentEncoding,
		demEncoding:         format.encoding,
		tileCache:           newTileCache(s.tileCacheBytes),
		metadataPayload:     metadataPayload,
		metadataETag:        quoteETag(digest(metadataPayload)),
		metadataPayloadGzip: gzipIfSmaller(metadataPayload),
	}
	if s.publicBase != "" {
		manifest := offline.Manifest{
			FormatVersion:    offline.ProtocolVersion,
			Dataset:          s.datasetID,
			Revision:         gen.revision,
			CoordinateSystem: "TMS",
			TileURLTemplate:  s.publicBase + "/sync/tiles/{revision}/{z}/{x}/{y}",
			ContentType:      gen.contentType,
			ContentEncoding:  gen.contentEncoding,
			CreatedAt:        gen.createdAt,
		}
		gen.manifestPayload, err = json.Marshal(manifest)
		if err != nil {
			return nil, fmt.Errorf("tinytiles server: encode sync manifest: %w", err)
		}
		gen.manifestPayloadGzip = gzipIfSmaller(gen.manifestPayload)
		tileURL := s.xyzTileURL(s.publicBase, gen)
		gen.tileJSONPayload, err = json.Marshal(tileJSON(gen.metadata, tileURL, gen.revision, gen.contentType, gen.demEncoding))
		if err != nil {
			return nil, fmt.Errorf("tinytiles server: encode TileJSON: %w", err)
		}
		gen.tileJSONETag = quoteETag(digest(gen.tileJSONPayload))
		gen.tileJSONPayloadGzip = gzipIfSmaller(gen.tileJSONPayload)
	}
	return gen, nil
}

// gzipIfSmaller returns a gzip encoding of raw, or nil when the compressed
// form is not actually smaller. gzip has a fixed ~20-byte header/trailer
// overhead, so a tiny payload (an empty metadata map, for example) can come
// out larger compressed than raw; nil tells writeJSON to fall back to raw
// rather than serving a client bytes that "compression" made bigger.
func gzipIfSmaller(raw []byte) []byte {
	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := writer.Write(raw); err != nil {
		return nil
	}
	if err := writer.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(raw) {
		return nil
	}
	return buf.Bytes()
}

// Close stops predictive-cache workers. It does not close the Dataset, which
// remains owned by the caller. Close is idempotent and should run before the
// caller closes its Dataset during application shutdown.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.prefetchMu.Lock()
	if s.prefetchClosed {
		s.prefetchMu.Unlock()
		return
	}
	s.prefetchClosed = true
	prefetcher := s.prefetcher
	s.prefetchMu.Unlock()
	if prefetcher != nil {
		prefetcher.close()
	}
}

func (s *Server) routePrefetcher() *tilePrefetcher {
	s.prefetchMu.Lock()
	defer s.prefetchMu.Unlock()
	if s.prefetchClosed || s.prefetchWorkers < 1 || s.gen.Load().tileCache == nil {
		return nil
	}
	if s.prefetcher == nil {
		s.prefetcher = newTilePrefetcher(s, s.prefetchWorkers, s.prefetchQueue)
	}
	return s.prefetcher
}

// Handler exposes the complete standalone route set:
//
//   - /tiles/{z}/{x}/{y}[.<format>] is an XYZ slippy-map endpoint;
//   - /tilejson.json and /metadata expose standard consumer metadata;
//   - /sync/manifest.json and /sync/tiles/{revision}/{z}/{x}/{y} provide the
//     revisioned TMS cache protocol for native and WASM clients;
//   - /healthz returns 204 when this Server is configured.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/tiles/", http.StripPrefix("/tiles/", s.XYZHandler()))
	mux.Handle("/sync/", http.StripPrefix("/sync", s.syncHandler()))
	mux.HandleFunc("/tilejson.json", s.serveTileJSON)
	mux.HandleFunc("/metadata", s.serveMetadata)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return s.withCORS(mux)
}

// XYZHandler serves bare z/x/y[.<format>] paths. This makes it safe to mount in an
// existing service with http.StripPrefix("/tiles/", handler).
func (s *Server) XYZHandler() http.Handler {
	return http.HandlerFunc(s.serveXYZ)
}

// SyncHandler serves /manifest.json and /tiles/{revision}/{z}/{x}/{y}. It is
// mountable below a caller-selected path with http.StripPrefix.
func (s *Server) SyncHandler() http.Handler {
	return s.withCORS(s.syncHandler())
}

func (s *Server) syncHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", s.serveManifest)
	mux.HandleFunc("/tiles/", s.serveTMSSync)
	return mux
}

func (s *Server) serveXYZ(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	gen := s.gen.Load()
	z, x, y, err := parseXYZPathWithSuffix(request.URL.Path, gen.tilePathSuffix)
	if err != nil {
		http.Error(w, "invalid XYZ tile coordinate", http.StatusBadRequest)
		return
	}
	etag := tileCoordinateETag(gen.revision, ":xyz:", z, x, y)
	if writeConditionalHeaders(w, request, etag, "public, max-age=31536000, immutable") {
		return
	}
	// parseXYZPath has already validated z/x/y, so the row flip is safe here.
	key := tiles.Key{Z: z, X: x, Y: (1 << z) - 1 - y}
	tile, checksum, found, err := s.lookupTile(request.Context(), gen, key)
	if err != nil {
		http.Error(w, "tile lookup failed", http.StatusInternalServerError)
		return
	}
	if !found {
		setHeader(w.Header(), "Cache-Control", "public, max-age=60")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeTileHeaders(w, gen, tile.Data, checksum, true)
	if request.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(tile.Data)
}

func (s *Server) serveManifest(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	gen := s.gen.Load()
	if gen.manifestPayload != nil {
		writeJSON(w, request, gen.manifestPayload, gen.manifestPayloadGzip, quoteETag(gen.revision), "no-cache")
		return
	}
	baseURL, err := s.baseURL(request)
	if err != nil {
		http.Error(w, "invalid public base URL", http.StatusBadRequest)
		return
	}
	manifest := offline.Manifest{
		FormatVersion:    offline.ProtocolVersion,
		Dataset:          s.datasetID,
		Revision:         gen.revision,
		CoordinateSystem: "TMS",
		TileURLTemplate:  baseURL + "/sync/tiles/{revision}/{z}/{x}/{y}",
		ContentType:      gen.contentType,
		ContentEncoding:  gen.contentEncoding,
		CreatedAt:        gen.createdAt,
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		http.Error(w, "encode sync manifest", http.StatusInternalServerError)
		return
	}
	writeJSON(w, request, payload, nil, quoteETag(gen.revision), "no-cache")
}

func (s *Server) serveTMSSync(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	gen := s.gen.Load()
	parts := strings.Split(strings.TrimPrefix(path.Clean(request.URL.Path), "/tiles/"), "/")
	if len(parts) != 4 || parts[0] != gen.revision {
		http.NotFound(w, request)
		return
	}
	key, err := parseTMSParts(parts[1:])
	if err != nil {
		http.Error(w, "invalid TMS tile coordinate", http.StatusBadRequest)
		return
	}
	etag := tileCoordinateETag(gen.revision, ":tms:", key.Z, key.X, key.Y)
	if writeConditionalHeaders(w, request, etag, "public, max-age=31536000, immutable") {
		return
	}
	tile, checksum, found, err := s.lookupTile(request.Context(), gen, key)
	if err != nil {
		http.Error(w, "tile lookup failed", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, request)
		return
	}
	// Do not set HTTP Content-Encoding here. Browser Fetch may transparently
	// decode it, whereas the offline protocol stores and checksums exact raw
	// bytes. The protocol-specific header retains the representation instead.
	writeTileHeaders(w, gen, tile.Data, checksum, false)
	if request.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(tile.Data)
}

func (s *Server) serveTileJSON(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	gen := s.gen.Load()
	if gen.tileJSONPayload != nil {
		writeJSON(w, request, gen.tileJSONPayload, gen.tileJSONPayloadGzip, gen.tileJSONETag, "public, max-age=300, stale-while-revalidate=60")
		return
	}
	baseURL, err := s.baseURL(request)
	if err != nil {
		http.Error(w, "invalid public base URL", http.StatusBadRequest)
		return
	}
	tileURL := s.xyzTileURL(baseURL, gen)
	payload, err := json.Marshal(tileJSON(gen.metadata, tileURL, gen.revision, gen.contentType, gen.demEncoding))
	if err != nil {
		http.Error(w, "encode TileJSON", http.StatusInternalServerError)
		return
	}
	writeJSON(w, request, payload, nil, quoteETag(digest(payload)), "public, max-age=300, stale-while-revalidate=60")
}

func (s *Server) serveMetadata(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	gen := s.gen.Load()
	writeJSON(w, request, gen.metadataPayload, gen.metadataPayloadGzip, gen.metadataETag, "public, max-age=300, stale-while-revalidate=60")
}

func (s *Server) lookupTile(ctx context.Context, gen *generation, key tiles.Key) (tiles.Tile, string, bool, error) {
	// A deliberately disabled cache must retain the leanest possible direct
	// lookup path. Coalescing is useful only when this Server can retain the
	// resulting immutable payload or checksum for later requests.
	if gen.tileCache == nil {
		data, checksum, found, err := loadTile(ctx, gen, key)
		if err != nil || !found {
			return tiles.Tile{}, "", found, err
		}
		return tiles.Tile{Key: key, Data: data}, checksum, true, nil
	}
	if data, checksum, found := gen.tileCache.get(key); found {
		return tiles.Tile{Key: key, Data: data}, checksum, true, nil
	}
	data, checksum, found, err := gen.tileLoads.do(ctx, key, func(loadCtx context.Context) ([]byte, string, bool, error) {
		return loadTile(loadCtx, gen, key)
	})
	if err != nil || !found {
		return tiles.Tile{}, "", found, err
	}
	return tiles.Tile{Key: key, Data: data}, checksum, true, nil
}

func loadTile(ctx context.Context, gen *generation, key tiles.Key) ([]byte, string, bool, error) {
	// A competing request can have filled the cache between the initial lookup
	// and becoming this key's loader.
	if data, checksum, found := gen.tileCache.get(key); found {
		return data, checksum, true, nil
	}
	checksum, checksumFound := gen.tileCache.checksum(key)
	tile, found, err := gen.dataset.LookupTMS(ctx, key)
	if err != nil || !found {
		return nil, "", found, err
	}
	if !checksumFound {
		checksum = offline.Checksum(tile.Data)
	}
	gen.tileCache.put(key, tile.Data, checksum)
	return tile.Data, checksum, true, nil
}

func writeTileHeaders(w http.ResponseWriter, gen *generation, data []byte, checksum string, browserTile bool) {
	header := w.Header()
	setHeader(header, "Content-Type", gen.contentType)
	if gen.contentEncoding != "" {
		setHeader(header, headerTileContentEncoding, gen.contentEncoding)
		if browserTile {
			setHeader(header, "Content-Encoding", gen.contentEncoding)
		}
	}
	setHeader(header, headerTileChecksum, checksum)
	setHeader(header, "Content-Length", strconv.Itoa(len(data)))
}

// corsPreflightMaxAgeSeconds is the longest a browser will actually cache a
// preflight for (Chromium and Firefox both cap it at this value regardless of
// a larger Access-Control-Max-Age), so tinyTiles asks for exactly that rather
// than a smaller value that would just cost a client extra round trips. A map
// client commonly fires many small concurrent cross-origin tile requests
// while panning or zooming; without this header a browser may re-run the
// OPTIONS preflight far more often than the actual CORS policy ever changes.
const corsPreflightMaxAgeSeconds = "86400"

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if s.corsOrigin != "" {
			header := w.Header()
			setHeader(header, "Access-Control-Allow-Origin", s.corsOrigin)
			setHeader(header, "Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			setHeader(header, "Access-Control-Allow-Headers", "Content-Type, If-None-Match")
			setHeader(header, "Access-Control-Expose-Headers", offline.HeaderTileChecksum+", "+offline.HeaderTileContentEncoding+", ETag")
			setHeader(header, "Access-Control-Max-Age", corsPreflightMaxAgeSeconds)
		}
		if request.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, request)
	})
}

func (s *Server) baseURL(request *http.Request) (string, error) {
	if s.publicBase != "" {
		return s.publicBase, nil
	}
	scheme := "http"
	if request != nil && request.TLS != nil {
		scheme = "https"
	}
	host := ""
	if request != nil {
		host = request.Host
	}
	candidate := scheme + "://" + host
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" || parsed.Host != host || parsed.Path != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("request host is not a valid origin")
	}
	return candidate + s.mountPath, nil
}

// xyzTileURL returns the canonical cache-busting XYZ URL advertised in
// TileJSON. Its extension is selected from the generation's MBTiles metadata.
func (s *Server) xyzTileURL(baseURL string, gen *generation) string {
	return baseURL + "/tiles/{z}/{x}/{y}." + gen.tileExtension + "?tinytiles_rev=" + url.QueryEscape(gen.revision)
}

func validateDatasetID(dataset string) error {
	manifest := offline.Manifest{FormatVersion: offline.ProtocolVersion, Dataset: dataset, Revision: strings.Repeat("0", sha256.Size*2), CoordinateSystem: "TMS", TileURLTemplate: "https://example.invalid/{z}/{x}/{y}"}
	return manifest.Validate()
}

func normalizePublicBase(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("tinytiles server: public base must be an absolute http(s) URL without credentials, query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// normalizeMountPath accepts only a clean, absolute URL path. Unlike
// PublicBase it intentionally cannot name an origin, query, fragment, or
// encoded path separator: it describes the local mux mount, not an arbitrary
// remote URL. A single trailing slash is normalized away so every generated
// URL has exactly one separator before /tiles or /sync.
func normalizeMountPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return "", nil
	}
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "//") || strings.ContainsAny(value, "?#\\%") {
		return "", errors.New("tinytiles server: mount path must be a clean absolute path")
	}
	value = strings.TrimRight(value, "/")
	if value == "" {
		return "", nil
	}
	if path.Clean(value) != value {
		return "", errors.New("tinytiles server: mount path must be a clean absolute path")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != value {
		return "", errors.New("tinytiles server: mount path must be a clean absolute path")
	}
	return value, nil
}

func normalizeCORSOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "*" {
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("tinytiles server: cors origin must be * or an http(s) origin without path, credentials, query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func parseXYZPath(raw string) (int, int, int, error) {
	return parseXYZPathWithSuffix(raw, "."+defaultTileExtension)
}

func parseXYZPathWithExtension(raw, extension string) (int, int, int, error) {
	return parseXYZPathWithSuffix(raw, "."+extension)
}

func parseXYZPathWithSuffix(raw, suffix string) (int, int, int, error) {
	// URL.Path has already been separated from the query string by net/http.
	// Keep the normal z/x/y[.<format>] path allocation-free: this is on every XYZ
	// request, while accepting cleaned variants such as "2/./1/1" buys neither
	// a useful route nor an unambiguous cache key.
	raw = strings.TrimPrefix(raw, "/")
	if strings.HasSuffix(raw, suffix) {
		raw = raw[:len(raw)-len(suffix)]
	}
	firstSlash := strings.IndexByte(raw, '/')
	if firstSlash <= 0 {
		return 0, 0, 0, errXYZPath
	}
	secondStart := firstSlash + 1
	secondSlashOffset := strings.IndexByte(raw[secondStart:], '/')
	if secondSlashOffset <= 0 {
		return 0, 0, 0, errXYZPath
	}
	secondSlash := secondStart + secondSlashOffset
	thirdStart := secondSlash + 1
	if thirdStart >= len(raw) || strings.IndexByte(raw[thirdStart:], '/') >= 0 {
		return 0, 0, 0, errXYZPath
	}
	z, err := parseXYZNumber(raw[:firstSlash])
	if err != nil {
		return 0, 0, 0, err
	}
	x, err := parseXYZNumber(raw[secondStart:secondSlash])
	if err != nil {
		return 0, 0, 0, err
	}
	y, err := parseXYZNumber(raw[thirdStart:])
	if err != nil {
		return 0, 0, 0, err
	}
	if err := (tiles.Key{Z: z, X: x, Y: y}).Validate(); err != nil {
		return 0, 0, 0, err
	}
	return z, x, y, nil
}

// parseXYZNumber parses a coordinate without strconv's generalized syntax.
// Coordinates are non-negative decimal integers and never exceed 2^30, so an
// early bound avoids both integer overflow and unnecessary long-path work.
func parseXYZNumber(raw string) (int, error) {
	if raw == "" {
		return 0, errXYZCoordinate
	}
	const maxCoordinate = 1 << 30
	value := 0
	for index := 0; index < len(raw); index++ {
		digit := raw[index] - '0'
		if digit > 9 || value > (maxCoordinate-int(digit))/10 {
			return 0, errXYZCoordinate
		}
		value = value*10 + int(digit)
	}
	return value, nil
}

func parseTMSParts(parts []string) (tiles.Key, error) {
	z, x, y, err := parseCoordinates(parts)
	if err != nil {
		return tiles.Key{}, err
	}
	return tiles.Key{Z: z, X: x, Y: y}, nil
}

func parseCoordinates(parts []string) (int, int, int, error) {
	if len(parts) != 3 {
		return 0, 0, 0, errors.New("need z/x/y")
	}
	values := [3]int{}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return 0, 0, 0, err
		}
		values[index] = value
	}
	if err := (tiles.Key{Z: values[0], X: values[1], Y: values[2]}).Validate(); err != nil {
		return 0, 0, 0, err
	}
	return values[0], values[1], values[2], nil
}

func writeConditionalHeaders(w http.ResponseWriter, request *http.Request, etag, cacheControl string) bool {
	header := w.Header()
	setHeader(header, headerETag, etag)
	setHeader(header, "Cache-Control", cacheControl)
	if request.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// writeJSON serves payload, or gzipPayload when the client's Accept-Encoding
// allows gzip and buildGeneration found a precomputed encoding worth using.
// gzipPayload may be nil: the dynamic per-request fallback paths (no
// PublicBase configured, so the payload cannot be precomputed) simply skip
// compression rather than paying a per-request gzip cost for an
// already-uncommon configuration.
func writeJSON(w http.ResponseWriter, request *http.Request, payload, gzipPayload []byte, etag, cacheControl string) {
	header := w.Header()
	setHeader(header, "Content-Type", "application/json; charset=utf-8")
	body := payload
	if gzipPayload != nil {
		// A cache or proxy sitting between tinyTiles and the client must not
		// serve one client's gzip response to another client that cannot
		// decode it, or vice versa.
		setHeader(header, "Vary", "Accept-Encoding")
		if acceptsGzip(request) {
			setHeader(header, "Content-Encoding", "gzip")
			body = gzipPayload
		}
	}
	setHeader(header, "Content-Length", strconv.Itoa(len(body)))
	if writeConditionalHeaders(w, request, etag, cacheControl) || request.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// acceptsGzip reports whether the client's Accept-Encoding header allows a
// gzip response. A missing header is treated as "no", the common defensive
// choice (matching nginx's and Apache's default gzip modules): an RFC 7231
// reader could argue a missing header permits any coding, but an unusual
// client that omits Accept-Encoding entirely is exactly the client least
// likely to expect a compressed body back.
func acceptsGzip(request *http.Request) bool {
	header := request.Header.Get("Accept-Encoding")
	if header == "" {
		return false
	}
	sawGzip, gzipAllowed := false, false
	// Per RFC 7231 §5.3.4, a coding absent from a present Accept-Encoding
	// field is *not* acceptable unless "*" says otherwise: an explicit
	// "Accept-Encoding: identity" is the standard way a client asks for an
	// uncompressed body, and gzip must not be forced on it.
	sawWildcard, wildcardAllowed := false, false
	for _, part := range strings.Split(header, ",") {
		token, quality := parseAcceptEncodingToken(part)
		switch token {
		case "gzip":
			sawGzip, gzipAllowed = true, quality != 0
		case "*":
			sawWildcard, wildcardAllowed = true, quality != 0
		}
	}
	if sawGzip {
		return gzipAllowed
	}
	return sawWildcard && wildcardAllowed
}

// parseAcceptEncodingToken splits one comma-separated Accept-Encoding item
// into its lowercase coding name and quality value (default 1, per RFC 7231
// §5.3.1). "gzip;q=0" is the standard way a client excludes an otherwise
// acceptable coding, so the quality is what tinyTiles must honor, not just
// the coding name's presence.
func parseAcceptEncodingToken(part string) (token string, quality float64) {
	quality = 1
	fields := strings.Split(part, ";")
	token = strings.ToLower(strings.TrimSpace(fields[0]))
	for _, param := range fields[1:] {
		value, found := strings.CutPrefix(strings.TrimSpace(param), "q=")
		if !found {
			continue
		}
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			quality = parsed
		}
	}
	return token, quality
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	setHeader(w.Header(), "Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// setHeader exactly matches http.Header.Set's replacement semantics, but its
// caller supplies a canonical key. This avoids repeated MIME-key parsing in
// every cached-tile response.
func setHeader(header http.Header, canonicalKey, value string) {
	header[canonicalKey] = []string{value}
}

func quoteETag(value string) string { return `"` + value + `"` }

// tileCoordinateETag formats the immutable per-tile validator in one buffer.
// The usual string-concatenation form allocates intermediate decimal strings
// for z/x/y on every cached response. New validates revisions as SHA-256 hex,
// so 128 bytes covers every valid coordinate without a grow allocation.
func tileCoordinateETag(revision, scheme string, z, x, y int) string {
	var buffer [128]byte
	value := buffer[:0]
	value = append(value, '"')
	value = append(value, revision...)
	value = append(value, scheme...)
	value = strconv.AppendInt(value, int64(z), 10)
	value = append(value, '/')
	value = strconv.AppendInt(value, int64(x), 10)
	value = append(value, '/')
	value = strconv.AppendInt(value, int64(y), 10)
	value = append(value, '"')
	return string(value)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func tileJSON(metadata map[string]string, tileURL, revision, contentType, demEncoding string) map[string]any {
	minZoom := integerMetadata(metadata, "minzoom", 0)
	maxZoom := integerMetadata(metadata, "maxzoom", 22)
	attribution := strings.TrimSpace(metadata["attribution"])
	if attribution == "" {
		attribution = "© OpenStreetMap contributors"
	}
	result := map[string]any{
		"tilejson":           "3.0.0",
		"name":               metadata["name"],
		"description":        metadata["description"],
		"version":            metadata["version"],
		"attribution":        attribution,
		"scheme":             "xyz",
		"tiles":              []string{tileURL},
		"minzoom":            minZoom,
		"maxzoom":            maxZoom,
		"bounds":             floatListMetadata(metadata["bounds"], 4),
		"center":             floatListMetadata(metadata["center"], 3),
		"format":             metadata["format"],
		"tinytiles:revision": revision,
	}
	// The standard MBTiles vector-tileset metadata row (key "json") carries
	// vector_layers and, optionally, tilestats as an embedded JSON string —
	// see https://github.com/mapbox/mbtiles-spec. tinyTiles relays it
	// unchanged into the top-level TileJSON fields every vector-tile frontend
	// (MapLibre GL JS, Mapbox GL JS, OpenLayers, Maputnik, deck.gl, ...)
	// expects it in, rather than inventing layer semantics itself: the
	// generator that wrote the source MBTiles already owns that content.
	// It is only meaningful, and only sent, for a vector tileset.
	if contentType == vectorTileContentType {
		if vectorLayers, tilestats := vectorTilesetMetadata(metadata["json"]); vectorLayers != nil {
			result["vector_layers"] = vectorLayers
			if tilestats != nil {
				result["tilestats"] = tilestats
			}
		}
	}
	// A raster DEM tileset is an ordinary PNG/WebP on the wire; only this
	// field distinguishes elevation data from a plain raster, so a client can
	// build a terrain/hillshade source instead of rendering the pixels
	// literally. inferTileFormat has already cleared it for vector tilesets.
	if demEncoding != "" {
		result["encoding"] = demEncoding
	}
	return result
}

// vectorTilesetMetadata parses the MBTiles "json" metadata value. A missing
// or malformed value is not an error: TileJSON generation must not fail
// because an optional, best-effort field could not be relayed.
func vectorTilesetMetadata(rawJSON string) (vectorLayers []any, tilestats any) {
	rawJSON = strings.TrimSpace(rawJSON)
	if rawJSON == "" {
		return nil, nil
	}
	var decoded struct {
		VectorLayers []any `json:"vector_layers"`
		Tilestats    any   `json:"tilestats"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &decoded); err != nil {
		return nil, nil
	}
	return decoded.VectorLayers, decoded.Tilestats
}

func integerMetadata(metadata map[string]string, name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(metadata[name]))
	if err != nil {
		return fallback
	}
	return value
}

func floatListMetadata(value string, count int) []float64 {
	parts := strings.Split(value, ",")
	if len(parts) != count {
		return nil
	}
	result := make([]float64, 0, count)
	for _, part := range parts {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil
		}
		result = append(result, parsed)
	}
	return result
}
