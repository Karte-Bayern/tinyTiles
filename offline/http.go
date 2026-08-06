package offline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	defaultMaxManifestBytes int64 = 1 << 20
	defaultMaxTileBytes     int64 = 16 << 20
	tileAcceptHeader              = "application/vnd.mapbox-vector-tile, image/*;q=0.9, application/octet-stream;q=0.8"

	// HeaderTileChecksum is the SHA-256 of the exact raw tile bytes stored by
	// the client. It is intentionally a response header rather than part of
	// the URL, so revisioned URLs remain immutable and cache friendly.
	HeaderTileChecksum = "X-TinyTiles-SHA256"
	// HeaderTileContentEncoding describes the encoding of the raw tile payload
	// (for example a gzip-compressed MVT). It is not the HTTP
	// Content-Encoding header: browsers may transparently decode that header
	// before WebAssembly receives a response body.
	HeaderTileContentEncoding = "X-TinyTiles-Content-Encoding"
)

// HTTPFetcher implements the public server/client sync protocol. It accepts
// HTTP(S) manifest URLs and bounds every response before allocating its body.
// In a browser compiled to GOOS=js/GOARCH=wasm Go's HTTP transport uses Fetch;
// the remote server must therefore allow the relevant CORS origin.
type HTTPFetcher struct {
	ManifestURL     string
	Client          *http.Client
	Headers         http.Header
	MaxManifestSize int64
	MaxTileSize     int64
}

func (f *HTTPFetcher) FetchManifest(ctx context.Context) (Manifest, error) {
	endpoint, err := f.resolveManifestURL()
	if err != nil {
		return Manifest{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Manifest{}, err
	}
	request.Header.Set("Accept", "application/json")
	f.applyHeaders(request)
	response, err := f.httpClient().Do(request)
	if err != nil {
		return Manifest{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Manifest{}, httpStatusError("fetch manifest", response)
	}
	body, err := readLimitedHint(response.Body, f.maxManifestSize(), response.ContentLength)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest JSON: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	if resolved, err := resolveTemplateURL(endpoint, manifest.TileURLTemplate); err != nil {
		return Manifest{}, err
	} else {
		manifest.TileURLTemplate = resolved
	}
	return manifest, nil
}

func resolveTemplateURL(base *url.URL, raw string) (string, error) {
	// net/url correctly escapes literal braces. Replace placeholders with safe
	// sentinel path fragments while resolving a relative URL, then restore them.
	replacements := []struct{ placeholder, sentinel string }{
		{"{revision}", "__tinytiles_revision__"},
		{"{z}", "__tinytiles_z__"},
		{"{x}", "__tinytiles_x__"},
		{"{y}", "__tinytiles_y__"},
	}
	prepared := raw
	for _, replacement := range replacements {
		prepared = strings.ReplaceAll(prepared, replacement.placeholder, replacement.sentinel)
	}
	parsed, err := url.Parse(prepared)
	if err != nil {
		return "", fmt.Errorf("parse tile URL template: %w", err)
	}
	if !parsed.IsAbs() {
		parsed = base.ResolveReference(parsed)
	}
	resolved := parsed.String()
	for _, replacement := range replacements {
		resolved = strings.ReplaceAll(resolved, replacement.sentinel, replacement.placeholder)
	}
	return resolved, nil
}

func (f *HTTPFetcher) FetchTile(ctx context.Context, manifest Manifest, key TileKey) (Tile, error) {
	endpoint, err := manifest.TileURL(key)
	if err != nil {
		return Tile{}, err
	}
	return f.fetchTileURL(ctx, manifest, endpoint)
}

// fetchVerifiedTile is the Synchronizer-only fast path. Sync has already
// validated the manifest and every requested key before workers start, while
// FetchManifest on this exact built-in fetcher has resolved relative templates
// to an absolute URL. Avoiding the repeated validation and template parse is
// measurable for large local/LAN syncs; fetchTileURL still validates the final
// HTTP(S) destination through net/http before sending it.
func (f *HTTPFetcher) fetchVerifiedTile(ctx context.Context, manifest Manifest, key TileKey) (Tile, error) {
	return f.fetchTileURL(ctx, manifest, renderTileURL(manifest.TileURLTemplate, manifest.Revision, key))
}

func (f *HTTPFetcher) fetchTileURL(ctx context.Context, manifest Manifest, endpoint string) (Tile, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Tile{}, fmt.Errorf("parse tile URL: %w", err)
	}
	if (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.Host == "" {
		return Tile{}, fmt.Errorf("unsupported tile URL scheme %q", request.URL.Scheme)
	}
	request.Header.Set("Accept", tileAcceptHeader)
	f.applyHeaders(request)
	response, err := f.httpClient().Do(request)
	if err != nil {
		return Tile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Tile{}, httpStatusError("fetch tile", response)
	}
	if response.ContentLength > f.maxTileSize() {
		return Tile{}, fmt.Errorf("tile response declares %d bytes, above limit %d", response.ContentLength, f.maxTileSize())
	}
	data, err := readLimitedHint(response.Body, f.maxTileSize(), response.ContentLength)
	if err != nil {
		return Tile{}, fmt.Errorf("read tile: %w", err)
	}
	tile := Tile{
		Data:            data,
		ContentType:     response.Header.Get("Content-Type"),
		ContentEncoding: response.Header.Get(HeaderTileContentEncoding),
		ETag:            response.Header.Get("ETag"),
		Checksum:        strings.TrimSpace(response.Header.Get(HeaderTileChecksum)),
	}
	// Keep native integrations with existing endpoints working, but new sync
	// endpoints should always use HeaderTileContentEncoding. HTTP content
	// encodings are not a portable representation boundary for browsers.
	if tile.ContentEncoding == "" {
		tile.ContentEncoding = response.Header.Get("Content-Encoding")
	}
	if tile.ContentType == "" {
		tile.ContentType = manifest.ContentType
	}
	if tile.ContentEncoding == "" {
		tile.ContentEncoding = manifest.ContentEncoding
	}
	if err := verifyTile(tile); err != nil {
		return Tile{}, err
	}
	return tile, nil
}

func (f *HTTPFetcher) resolveManifestURL() (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(f.ManifestURL))
	if err != nil {
		return nil, fmt.Errorf("parse manifest URL: %w", err)
	}
	if (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("unsupported manifest URL scheme %q", endpoint.Scheme)
	}
	return endpoint, nil
}

func (f *HTTPFetcher) httpClient() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return defaultHTTPFetcherClient()
}

func (f *HTTPFetcher) maxManifestSize() int64 {
	if f.MaxManifestSize > 0 {
		return f.MaxManifestSize
	}
	return defaultMaxManifestBytes
}

func (f *HTTPFetcher) maxTileSize() int64 {
	if f.MaxTileSize > 0 {
		return f.MaxTileSize
	}
	return defaultMaxTileBytes
}

func (f *HTTPFetcher) applyHeaders(request *http.Request) {
	for name, values := range f.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
}

func readLimited(reader io.Reader, max int64) ([]byte, error) {
	return readLimitedHint(reader, max, -1)
}

// readLimitedHint is the bounded response reader used for normal HTTP bodies.
// Content-Length is already supplied and checked by net/http, so reserving it
// upfront avoids io.ReadAll's repeated growth and final copy for ordinary tile
// responses. An unknown or untrusted hint falls back to the same bounded
// streaming behavior.
func readLimitedHint(reader io.Reader, max, hint int64) ([]byte, error) {
	if max <= 0 {
		return nil, errors.New("response size limit must be positive")
	}
	if hint > max {
		return nil, fmt.Errorf("response declares %d bytes, above limit %d", hint, max)
	}
	limit := max
	const maxInt64 = int64(^uint64(0) >> 1)
	if limit < maxInt64 {
		limit++
	}
	if hint < 0 {
		data, err := io.ReadAll(io.LimitReader(reader, limit))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, fmt.Errorf("response exceeds %d bytes", max)
		}
		return data, nil
	}
	maxInt := int64(^uint(0) >> 1)
	if hint <= maxInt {
		// A fixed Content-Length lets us allocate the final payload exactly.
		// bytes.Buffer.Grow rounds an 8 KiB tile to a much larger backing array
		// on several targets, which used to dominate the sync allocation rate.
		// The small tail check retains the old behavior for a broken custom
		// RoundTripper whose body is longer than its declared length.
		data := make([]byte, int(hint))
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		return readDeclaredResponseTail(reader, data, max)
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return data, nil
}

// readDeclaredResponseTail preserves the bounded-reader behavior for a
// malformed custom Response.Body that contains bytes after Content-Length.
// Standard net/http response bodies end at their declared length, so this
// normally performs one allocation-free EOF read.
func readDeclaredResponseTail(reader io.Reader, data []byte, max int64) ([]byte, error) {
	var first [1]byte
	n, err := reader.Read(first[:])
	if n == 0 {
		if err == nil || errors.Is(err, io.EOF) {
			return data, nil
		}
		return nil, err
	}
	if int64(len(data))+int64(n) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	data = append(data, first[:n]...)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		return nil, err
	}
	remaining := max - int64(len(data))
	if remaining < int64(^uint64(0)>>1) {
		remaining++
	}
	tail, err := io.ReadAll(io.LimitReader(reader, remaining))
	if err != nil {
		return nil, err
	}
	data = append(data, tail...)
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return data, nil
}

func httpStatusError(operation string, response *http.Response) error {
	body, _ := readLimited(response.Body, 1024)
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("%s: HTTP %d: %s", operation, response.StatusCode, message)
}

// Checksum returns the canonical digest emitted by the demo server and useful
// to custom Fetcher implementations that want end-to-end tile verification.
func Checksum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
