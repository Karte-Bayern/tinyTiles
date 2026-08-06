//go:build sqliteimport

package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeGzipBody decompresses a gzip response body and fails the test on any
// error, so a caller can assert on the decoded JSON directly.
func decodeGzipBody(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress response: %v", err)
	}
	return decoded
}

func TestTileJSONServesGzipWhenAccepted(t *testing.T) {
	server := testServer(t)

	plainRequest := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	plainResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(plainResponse, plainRequest)
	if got := plainResponse.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("request without Accept-Encoding got Content-Encoding = %q", got)
	}
	if got := plainResponse.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding even on the uncompressed response", got)
	}

	gzipRequest := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	gzipRequest.Header.Set("Accept-Encoding", "gzip")
	gzipResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(gzipResponse, gzipRequest)
	if got := gzipResponse.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := gzipResponse.Header().Get("Content-Length"); got != "" {
		if got == plainResponse.Header().Get("Content-Length") {
			t.Fatalf("gzip Content-Length %q equals the uncompressed length; compression was not applied", got)
		}
	}
	decoded := decodeGzipBody(t, gzipResponse.Body.Bytes())
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("decompressed body is not valid JSON: %v", err)
	}
	if payload["scheme"] != "xyz" {
		t.Fatalf("decompressed TileJSON = %#v", payload)
	}
	if !bytes.Equal(decoded, plainResponse.Body.Bytes()) {
		t.Fatal("gzip response decodes to different bytes than the uncompressed response")
	}

	// The resource is unchanged, so both encodings must share the same
	// validator; RFC 7232 permits a strong ETag shared across content-codings
	// of the same representation, and Vary already tells caches to key on
	// Accept-Encoding.
	if plainResponse.Header().Get("ETag") != gzipResponse.Header().Get("ETag") {
		t.Fatalf("ETag differs between encodings: plain=%q gzip=%q", plainResponse.Header().Get("ETag"), gzipResponse.Header().Get("ETag"))
	}
}

func TestTileJSONHonorsExplicitGzipExclusion(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	request.Header.Set("Accept-Encoding", "gzip;q=0, br")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none: client explicitly excluded gzip", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not plain JSON: %v", err)
	}
}

func TestGzipResponseStillHonorsConditionalGet(t *testing.T) {
	server := testServer(t)
	first := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	first.Header.Set("Accept-Encoding", "gzip")
	firstResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(firstResponse, first)

	conditional := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	conditional.Header.Set("Accept-Encoding", "gzip")
	conditional.Header.Set("If-None-Match", firstResponse.Header().Get("ETag"))
	conditionalResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(conditionalResponse, conditional)
	if conditionalResponse.Code != http.StatusNotModified {
		t.Fatalf("conditional gzip request status = %d, want 304", conditionalResponse.Code)
	}
	if conditionalResponse.Body.Len() != 0 {
		t.Fatalf("304 response has a %d-byte body, want empty", conditionalResponse.Body.Len())
	}
}

// TestMetadataAndManifestAlsoServeGzip uses testVectorDatasetWithLayers,
// whose metadata carries a vector_layers/tilestats blob big and repetitive
// enough to guarantee gzip actually helps. testServer's own fixture is small
// enough that gzip's ~20-byte frame overhead does not pay off — confirmed
// separately by TestGzipIfSmallerRejectsPayloadsCompressionWouldGrow — so it
// would not exercise this path reliably.
func TestMetadataAndManifestAlsoServeGzip(t *testing.T) {
	dataset := testVectorDatasetWithLayers(t)
	server, err := New(Config{Dataset: dataset, DatasetID: "fixture", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	for _, path := range []string{"/metadata", "/sync/manifest.json"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://tiles.example"+path, nil)
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("%s Content-Encoding = %q, want gzip", path, got)
			}
			decoded := decodeGzipBody(t, response.Body.Bytes())
			var payload map[string]any
			if err := json.Unmarshal(decoded, &payload); err != nil {
				t.Fatalf("%s decompressed body is not valid JSON: %v", path, err)
			}
		})
	}
}

func TestGzipIfSmallerRejectsPayloadsCompressionWouldGrow(t *testing.T) {
	if got := gzipIfSmaller([]byte("{}")); got != nil {
		t.Fatalf("gzipIfSmaller(tiny payload) = %v, want nil (gzip overhead exceeds the raw size)", got)
	}
	large := bytes.Repeat([]byte(`{"a":"b"},`), 500)
	got := gzipIfSmaller(large)
	if got == nil {
		t.Fatal("gzipIfSmaller(large, highly compressible payload) = nil, want a smaller encoding")
	}
	if len(got) >= len(large) {
		t.Fatalf("gzipIfSmaller result is %d bytes, not smaller than the %d-byte input", len(got), len(large))
	}
}

func TestAcceptsGzipParsesQualityValues(t *testing.T) {
	for _, test := range []struct {
		header string
		want   bool
	}{
		{header: "", want: false},
		{header: "gzip", want: true},
		{header: "gzip, deflate, br", want: true},
		{header: "GZIP", want: true},
		{header: "gzip;q=0", want: false},
		{header: "gzip;q=0.0", want: false},
		{header: "gzip;q=0, br", want: false},
		{header: "*", want: true},
		{header: "*;q=0", want: false},
		{header: "*;q=0, gzip", want: true},
		{header: "identity", want: false},
		{header: "br;q=1.0, gzip;q=0.5", want: true},
	} {
		request := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
		if test.header != "" {
			request.Header.Set("Accept-Encoding", test.header)
		}
		if got := acceptsGzip(request); got != test.want {
			t.Errorf("acceptsGzip(%q) = %t, want %t", test.header, got, test.want)
		}
	}
}

func TestCORSPreflightAdvertisesMaxAge(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodOptions, "https://tiles.example/tilejson.json", nil)
	request.Header.Set("Origin", "http://localhost:8081")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", response.Code)
	}
	if got := response.Header().Get("Access-Control-Max-Age"); got != corsPreflightMaxAgeSeconds {
		t.Fatalf("Access-Control-Max-Age = %q, want %q", got, corsPreflightMaxAgeSeconds)
	}
}
