// Package server exposes a mountable HTTP surface for a tinyTiles Dataset.
// It deliberately owns no listener, TLS configuration, authentication, or
// deployment policy. Use Handler with an existing application mux, or use the
// cmd/tinytiles-server binary for the small standalone application.
package server

import (
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
	"time"

	tinytiles "github.com/Karte-Bayern/tinyTiles"
	"github.com/Karte-Bayern/tinyTiles/offline"
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
	PublicBase string
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

// Server is a handler factory for one immutable dataset revision. It is safe
// for concurrent requests as long as its Dataset remains open.
type Server struct {
	dataset          *tinytiles.Dataset
	datasetID        string
	revision         string
	createdAt        time.Time
	metadata         map[string]string
	contentType      string
	tileExtension    string
	tilePathSuffix   string
	contentEncoding  string
	publicBase       string
	corsOrigin       string
	tileCache        *tileCache
	tileLoads        tileLoadGroup
	prefetchWorkers  int
	prefetchQueue    int
	prefetchMaxTiles int
	prefetchMu       sync.Mutex
	prefetcher       *tilePrefetcher
	prefetchClosed   bool
	metadataPayload  []byte
	metadataETag     string
	manifestPayload  []byte
	tileJSONPayload  []byte
	tileJSONETag     string
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
	info := config.Dataset.Info()
	if len(info.TileDigestSHA256) != sha256.Size*2 {
		return nil, errors.New("tinytiles server: artifact has no usable tile digest revision")
	}
	if _, err := hex.DecodeString(info.TileDigestSHA256); err != nil {
		return nil, fmt.Errorf("tinytiles server: invalid tile digest revision: %w", err)
	}
	metadata, err := config.Dataset.Metadata()
	if err != nil {
		return nil, fmt.Errorf("tinytiles server: read metadata: %w", err)
	}
	format, err := inferTileFormat(metadata, config.ContentType, config.TileExtension)
	if err != nil {
		return nil, err
	}
	contentType := format.contentType
	contentEncoding := strings.TrimSpace(config.ContentEncoding)
	if contentEncoding == "" && strings.EqualFold(metadata["kb:content_encoding"], "gzip") {
		contentEncoding = "gzip"
	}
	metadataPayload, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("tinytiles server: encode metadata: %w", err)
	}
	server := &Server{
		dataset:          config.Dataset,
		datasetID:        config.DatasetID,
		revision:         info.TileDigestSHA256,
		createdAt:        info.CreatedAt,
		metadata:         metadata,
		contentType:      contentType,
		tileExtension:    format.extension,
		tilePathSuffix:   "." + format.extension,
		contentEncoding:  contentEncoding,
		publicBase:       publicBase,
		corsOrigin:       corsOrigin,
		tileCache:        newTileCache(tileCacheBytes),
		prefetchWorkers:  prefetchWorkers,
		prefetchQueue:    prefetchQueue,
		prefetchMaxTiles: prefetchMaxTiles,
		metadataPayload:  metadataPayload,
		metadataETag:     quoteETag(digest(metadataPayload)),
	}
	if publicBase != "" {
		manifest := offline.Manifest{
			FormatVersion:    offline.ProtocolVersion,
			Dataset:          server.datasetID,
			Revision:         server.revision,
			CoordinateSystem: "TMS",
			TileURLTemplate:  publicBase + "/sync/tiles/{revision}/{z}/{x}/{y}",
			ContentType:      server.contentType,
			ContentEncoding:  server.contentEncoding,
			CreatedAt:        server.createdAt,
		}
		server.manifestPayload, err = json.Marshal(manifest)
		if err != nil {
			return nil, fmt.Errorf("tinytiles server: encode sync manifest: %w", err)
		}
		tileURL := server.xyzTileURL(publicBase)
		server.tileJSONPayload, err = json.Marshal(tileJSON(server.metadata, tileURL, server.revision))
		if err != nil {
			return nil, fmt.Errorf("tinytiles server: encode TileJSON: %w", err)
		}
		server.tileJSONETag = quoteETag(digest(server.tileJSONPayload))
	}
	return server, nil
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
	if s.prefetchClosed || s.prefetchWorkers < 1 || s.tileCache == nil {
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
// existing service with http.StripPrefix("/tiles/", handler), which is the
// integration shape used by Karte.Bayern-style application servers.
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
	z, x, y, err := parseXYZPathWithSuffix(request.URL.Path, s.tilePathSuffix)
	if err != nil {
		http.Error(w, "invalid XYZ tile coordinate", http.StatusBadRequest)
		return
	}
	etag := tileCoordinateETag(s.revision, ":xyz:", z, x, y)
	if writeConditionalHeaders(w, request, etag, "public, max-age=31536000, immutable") {
		return
	}
	// parseXYZPath has already validated z/x/y, so the row flip is safe here.
	key := tiles.Key{Z: z, X: x, Y: (1 << z) - 1 - y}
	tile, checksum, found, err := s.lookupTile(request.Context(), key)
	if err != nil {
		http.Error(w, "tile lookup failed", http.StatusInternalServerError)
		return
	}
	if !found {
		setHeader(w.Header(), "Cache-Control", "public, max-age=60")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeTileHeaders(w, tile.Data, checksum, true)
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
	if s.manifestPayload != nil {
		writeJSON(w, request, s.manifestPayload, quoteETag(s.revision), "no-cache")
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
		Revision:         s.revision,
		CoordinateSystem: "TMS",
		TileURLTemplate:  baseURL + "/sync/tiles/{revision}/{z}/{x}/{y}",
		ContentType:      s.contentType,
		ContentEncoding:  s.contentEncoding,
		CreatedAt:        s.createdAt,
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		http.Error(w, "encode sync manifest", http.StatusInternalServerError)
		return
	}
	writeJSON(w, request, payload, quoteETag(s.revision), "no-cache")
}

func (s *Server) serveTMSSync(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path.Clean(request.URL.Path), "/tiles/"), "/")
	if len(parts) != 4 || parts[0] != s.revision {
		http.NotFound(w, request)
		return
	}
	key, err := parseTMSParts(parts[1:])
	if err != nil {
		http.Error(w, "invalid TMS tile coordinate", http.StatusBadRequest)
		return
	}
	etag := tileCoordinateETag(s.revision, ":tms:", key.Z, key.X, key.Y)
	if writeConditionalHeaders(w, request, etag, "public, max-age=31536000, immutable") {
		return
	}
	tile, checksum, found, err := s.lookupTile(request.Context(), key)
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
	s.writeTileHeaders(w, tile.Data, checksum, false)
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
	if s.tileJSONPayload != nil {
		writeJSON(w, request, s.tileJSONPayload, s.tileJSONETag, "public, max-age=300, stale-while-revalidate=60")
		return
	}
	baseURL, err := s.baseURL(request)
	if err != nil {
		http.Error(w, "invalid public base URL", http.StatusBadRequest)
		return
	}
	tileURL := s.xyzTileURL(baseURL)
	payload, err := json.Marshal(tileJSON(s.metadata, tileURL, s.revision))
	if err != nil {
		http.Error(w, "encode TileJSON", http.StatusInternalServerError)
		return
	}
	writeJSON(w, request, payload, quoteETag(digest(payload)), "public, max-age=300, stale-while-revalidate=60")
}

func (s *Server) serveMetadata(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	writeJSON(w, request, s.metadataPayload, s.metadataETag, "public, max-age=300, stale-while-revalidate=60")
}

func (s *Server) lookupTile(ctx context.Context, key tiles.Key) (tiles.Tile, string, bool, error) {
	// A deliberately disabled cache must retain the leanest possible direct
	// lookup path. Coalescing is useful only when this Server can retain the
	// resulting immutable payload or checksum for later requests.
	if s.tileCache == nil {
		data, checksum, found, err := s.loadTile(ctx, key)
		if err != nil || !found {
			return tiles.Tile{}, "", found, err
		}
		return tiles.Tile{Key: key, Data: data}, checksum, true, nil
	}
	if data, checksum, found := s.tileCache.get(key); found {
		return tiles.Tile{Key: key, Data: data}, checksum, true, nil
	}
	data, checksum, found, err := s.tileLoads.do(ctx, key, func(loadCtx context.Context) ([]byte, string, bool, error) {
		return s.loadTile(loadCtx, key)
	})
	if err != nil || !found {
		return tiles.Tile{}, "", found, err
	}
	return tiles.Tile{Key: key, Data: data}, checksum, true, nil
}

func (s *Server) loadTile(ctx context.Context, key tiles.Key) ([]byte, string, bool, error) {
	// A competing request can have filled the cache between the initial lookup
	// and becoming this key's loader.
	if data, checksum, found := s.tileCache.get(key); found {
		return data, checksum, true, nil
	}
	checksum, checksumFound := s.tileCache.checksum(key)
	tile, found, err := s.dataset.LookupTMS(ctx, key)
	if err != nil || !found {
		return nil, "", found, err
	}
	if !checksumFound {
		checksum = offline.Checksum(tile.Data)
	}
	s.tileCache.put(key, tile.Data, checksum)
	return tile.Data, checksum, true, nil
}

func (s *Server) writeTileHeaders(w http.ResponseWriter, data []byte, checksum string, browserTile bool) {
	header := w.Header()
	setHeader(header, "Content-Type", s.contentType)
	if s.contentEncoding != "" {
		setHeader(header, headerTileContentEncoding, s.contentEncoding)
		if browserTile {
			setHeader(header, "Content-Encoding", s.contentEncoding)
		}
	}
	setHeader(header, headerTileChecksum, checksum)
	setHeader(header, "Content-Length", strconv.Itoa(len(data)))
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if s.corsOrigin != "" {
			header := w.Header()
			setHeader(header, "Access-Control-Allow-Origin", s.corsOrigin)
			setHeader(header, "Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			setHeader(header, "Access-Control-Allow-Headers", "Content-Type, If-None-Match")
			setHeader(header, "Access-Control-Expose-Headers", offline.HeaderTileChecksum+", "+offline.HeaderTileContentEncoding+", ETag")
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
	return candidate, nil
}

// xyzTileURL returns the canonical cache-busting XYZ URL advertised in
// TileJSON. Its extension is selected once from MBTiles metadata at startup.
func (s *Server) xyzTileURL(baseURL string) string {
	return baseURL + "/tiles/{z}/{x}/{y}." + s.tileExtension + "?tinytiles_rev=" + url.QueryEscape(s.revision)
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

func writeJSON(w http.ResponseWriter, request *http.Request, payload []byte, etag, cacheControl string) {
	header := w.Header()
	setHeader(header, "Content-Type", "application/json; charset=utf-8")
	setHeader(header, "Content-Length", strconv.Itoa(len(payload)))
	if writeConditionalHeaders(w, request, etag, cacheControl) || request.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(payload)
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

func tileJSON(metadata map[string]string, tileURL, revision string) map[string]any {
	minZoom := integerMetadata(metadata, "minzoom", 0)
	maxZoom := integerMetadata(metadata, "maxzoom", 22)
	attribution := strings.TrimSpace(metadata["attribution"])
	if attribution == "" {
		attribution = "© OpenStreetMap contributors"
	}
	return map[string]any{
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
