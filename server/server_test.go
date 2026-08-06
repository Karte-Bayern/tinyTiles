//go:build sqliteimport

package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tinytiles "github.com/Karte-Bayern/tinyTiles"
	"github.com/Karte-Bayern/tinyTiles/offline"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
	_ "modernc.org/sqlite"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dataset := testDataset(t)
	server, err := New(Config{Dataset: dataset, DatasetID: "fixture", PublicBase: "https://tiles.example", CORSOrigin: "http://localhost:8081"})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func testDataset(t *testing.T) *tinytiles.Dataset {
	t.Helper()
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "fixture.mbtiles")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata VALUES
			('name', 'fixture'), ('format', 'pbf'), ('kb:content_encoding', 'gzip'),
			('minzoom', '2'), ('maxzoom', '2'), ('bounds', '10,20,30,40');
		INSERT INTO tiles VALUES (2, 1, 2, x'010203');`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "fixture.ttiles")
	if _, err := tiles.ImportMBTiles(ctx, source, artifact, &tiles.ImportOptions{Schema: tiles.SchemaFlat, BatchSize: 1, MinFreeBytes: 0}); err != nil {
		t.Fatal(err)
	}
	dataset, err := tinytiles.Open(context.Background(), artifact, tinytiles.OpenOptions{Readers: 2, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataset.Close() })
	return dataset
}

func testRasterDataset(t *testing.T) *tinytiles.Dataset {
	t.Helper()
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "aerial.mbtiles")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata VALUES ('name', 'aerial fixture'), ('format', 'jpg'), ('minzoom', '2'), ('maxzoom', '2');
		INSERT INTO tiles VALUES (2, 1, 2, x'FFD8FF');`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "aerial.ttiles")
	if _, err := tiles.ImportMBTiles(ctx, source, artifact, &tiles.ImportOptions{Schema: tiles.SchemaFlat, BatchSize: 1, MinFreeBytes: 0}); err != nil {
		t.Fatal(err)
	}
	dataset, err := tinytiles.Open(context.Background(), artifact, tinytiles.OpenOptions{Readers: 2, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataset.Close() })
	return dataset
}

func TestHandlerServesXYZAndRevisionedTMS(t *testing.T) {
	server := testServer(t)
	xyzRequest := httptest.NewRequest(http.MethodGet, "https://tiles.example/tiles/2/1/1.mvt", nil)
	xyzResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(xyzResponse, xyzRequest)
	if xyzResponse.Code != http.StatusOK || string(xyzResponse.Body.Bytes()) != string([]byte{1, 2, 3}) {
		t.Fatalf("XYZ response = %d %q", xyzResponse.Code, xyzResponse.Body.Bytes())
	}
	if got := xyzResponse.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("XYZ Content-Encoding = %q", got)
	}
	if got := xyzResponse.Header().Get("Content-Type"); got != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("XYZ Content-Type = %q", got)
	}
	if got := xyzResponse.Header().Get(offline.HeaderTileChecksum); got != offline.Checksum([]byte{1, 2, 3}) {
		t.Fatalf("XYZ checksum = %q", got)
	}
	if got := xyzResponse.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8081" {
		t.Fatalf("CORS origin = %q", got)
	}
	if data, checksum, found := server.tileCache.get(tiles.Key{Z: 2, X: 1, Y: 2}); !found || string(data) != string([]byte{1, 2, 3}) || checksum != offline.Checksum([]byte{1, 2, 3}) {
		t.Fatalf("XYZ tile was not cached: bytes=%q checksum=%q found=%t", data, checksum, found)
	}

	manifestRequest := httptest.NewRequest(http.MethodGet, "https://tiles.example/sync/manifest.json", nil)
	manifestResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(manifestResponse, manifestRequest)
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("manifest status = %d: %s", manifestResponse.Code, manifestResponse.Body.String())
	}
	var manifest offline.Manifest
	if err := json.Unmarshal(manifestResponse.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.CoordinateSystem != "TMS" || !strings.Contains(manifest.TileURLTemplate, "/sync/tiles/{revision}/{z}/{x}/{y}") {
		t.Fatalf("manifest = %#v", manifest)
	}
	tmsRequest := httptest.NewRequest(http.MethodGet, "https://tiles.example/sync/tiles/"+manifest.Revision+"/2/1/2", nil)
	tmsResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(tmsResponse, tmsRequest)
	if tmsResponse.Code != http.StatusOK || string(tmsResponse.Body.Bytes()) != string([]byte{1, 2, 3}) {
		t.Fatalf("TMS response = %d %q", tmsResponse.Code, tmsResponse.Body.Bytes())
	}
	if got := tmsResponse.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("TMS must not use HTTP Content-Encoding, got %q", got)
	}
	if got := tmsResponse.Header().Get(offline.HeaderTileContentEncoding); got != "gzip" {
		t.Fatalf("TMS raw encoding = %q", got)
	}
}

func TestHandlerServesRasterTilesUsingMBTilesFormat(t *testing.T) {
	server, err := New(Config{Dataset: testRasterDataset(t), DatasetID: "aerial", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://tiles.example/tiles/2/1/1.jpg", nil))
	if response.Code != http.StatusOK || string(response.Body.Bytes()) != string([]byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("JPEG XYZ response = %d %q", response.Code, response.Body.Bytes())
	}
	if got := response.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("JPEG Content-Type = %q", got)
	}
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("JPEG Content-Encoding = %q", got)
	}

	tileJSONResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(tileJSONResponse, httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil))
	var tileJSON map[string]any
	if err := json.Unmarshal(tileJSONResponse.Body.Bytes(), &tileJSON); err != nil {
		t.Fatal(err)
	}
	tilesField, ok := tileJSON["tiles"].([]any)
	if !ok || len(tilesField) != 1 {
		t.Fatalf("raster TileJSON tiles = %#v", tileJSON["tiles"])
	}
	tileURL, ok := tilesField[0].(string)
	if !ok || !strings.Contains(tileURL, "/tiles/{z}/{x}/{y}.jpg?") {
		t.Fatalf("raster TileJSON URL = %#v", tilesField[0])
	}
}

func TestHandlerTileJSONAndConditionalResponses(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("TileJSON status = %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["scheme"] != "xyz" || payload["tinytiles:revision"] == "" {
		t.Fatalf("TileJSON = %#v", payload)
	}
	conditional := httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil)
	conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	server.Handler().ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("conditional TileJSON status = %d", notModified.Code)
	}
	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "https://tiles.example/tiles/2/1/0.mvt", nil))
	if missing.Code != http.StatusNoContent {
		t.Fatalf("missing XYZ status = %d", missing.Code)
	}
}

func TestConfigurationAndPathValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("nil dataset accepted")
	}
	for _, value := range []string{"ftp://tiles.example", "https://user@tiles.example", "https://tiles.example/?q=1"} {
		if _, err := normalizePublicBase(value); err == nil {
			t.Fatalf("public base accepted %q", value)
		}
	}
	for _, path := range []string{"2/1", "31/0/0", "2/4/1", "2/1/not-int"} {
		if _, _, _, err := parseXYZPath(path); err == nil {
			t.Fatalf("XYZ path accepted %q", path)
		}
	}
	dataset := testDataset(t)
	disabled, err := New(Config{Dataset: dataset, DatasetID: "fixture", TileCacheBytes: -1})
	if err != nil || disabled.tileCache != nil {
		t.Fatalf("disabled tile cache server=%#v err=%v", disabled, err)
	}
	if _, err := New(Config{Dataset: dataset, DatasetID: "fixture", TileCacheBytes: -2}); err == nil {
		t.Fatal("invalid tile cache budget accepted")
	}
	if _, err := New(Config{Dataset: dataset, DatasetID: "fixture", TileExtension: "png/extra"}); err == nil {
		t.Fatal("unsafe tile extension accepted")
	}
}
