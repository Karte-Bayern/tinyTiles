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

	tinytiles "github.com/Karte-Bayern/tinyTiles/v2"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// testVectorDatasetWithLayers mirrors testDataset but also writes the
// standard MBTiles vector-tileset "json" metadata row, so tileJSON has
// something real to relay as vector_layers/tilestats.
func testVectorDatasetWithLayers(t *testing.T) *tinytiles.Dataset {
	t.Helper()
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "fixture.mbtiles")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	vectorTilesetJSON := `{"vector_layers":[{"id":"roads","fields":{"class":"String"},"minzoom":8,"maxzoom":14}],"tilestats":{"layerCount":1}}`
	_, err = db.ExecContext(ctx, `
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata VALUES
			('name', 'fixture'), ('format', 'pbf'), ('minzoom', '2'), ('maxzoom', '2'), ('json', ?);
		INSERT INTO tiles VALUES (2, 1, 2, x'010203');`, vectorTilesetJSON)
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

func fetchTileJSON(t *testing.T, server *Server) map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://tiles.example/tilejson.json", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("TileJSON status = %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestTileJSONRelaysVectorLayersForVectorTileset(t *testing.T) {
	dataset := testVectorDatasetWithLayers(t)
	server, err := New(Config{Dataset: dataset, DatasetID: "fixture", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	payload := fetchTileJSON(t, server)
	layers, ok := payload["vector_layers"].([]any)
	if !ok || len(layers) != 1 {
		t.Fatalf("vector_layers = %#v", payload["vector_layers"])
	}
	layer, ok := layers[0].(map[string]any)
	if !ok || layer["id"] != "roads" {
		t.Fatalf("vector_layers[0] = %#v", layers[0])
	}
	stats, ok := payload["tilestats"].(map[string]any)
	if !ok || stats["layerCount"] != float64(1) {
		t.Fatalf("tilestats = %#v", payload["tilestats"])
	}
}

func TestTileJSONOmitsVectorLayersForRasterTileset(t *testing.T) {
	dataset := testRasterDataset(t)
	server, err := New(Config{Dataset: dataset, DatasetID: "fixture", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	payload := fetchTileJSON(t, server)
	if _, present := payload["vector_layers"]; present {
		t.Fatalf("raster TileJSON unexpectedly carries vector_layers: %#v", payload["vector_layers"])
	}
	if _, present := payload["tilestats"]; present {
		t.Fatalf("raster TileJSON unexpectedly carries tilestats: %#v", payload["tilestats"])
	}
}

// testDEMDataset builds a terrain fixture the way real DEM sources usually
// look: an ordinary PNG payload whose format row says nothing about
// elevation, plus the separate "encoding" metadata row that declares it.
func testDEMDataset(t *testing.T, format, encoding string) *tinytiles.Dataset {
	t.Helper()
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "dem.mbtiles")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata VALUES ('name', 'dem fixture'), ('format', ?), ('encoding', ?), ('minzoom', '2'), ('maxzoom', '2');
		INSERT INTO tiles VALUES (2, 1, 2, x'89504E47');`, format, encoding)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "dem.ttiles")
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

func TestRasterDEMTilesetServesRasterBytesAndAdvertisesEncoding(t *testing.T) {
	dataset := testDEMDataset(t, "png", "terrarium")
	server, err := New(Config{Dataset: dataset, DatasetID: "dem", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// The tile itself must stay a plain PNG: DEM-ness is metadata, not a
	// different wire representation.
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://tiles.example/tiles/2/1/1.png", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("DEM tile status = %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("DEM tile Content-Type = %q, want image/png", got)
	}

	payload := fetchTileJSON(t, server)
	if payload["encoding"] != "terrarium" {
		t.Fatalf("TileJSON encoding = %#v, want terrarium", payload["encoding"])
	}
	tilesField, ok := payload["tiles"].([]any)
	if !ok || len(tilesField) != 1 {
		t.Fatalf("DEM TileJSON tiles = %#v", payload["tiles"])
	}
	if url, ok := tilesField[0].(string); !ok || !strings.Contains(url, "/tiles/{z}/{x}/{y}.png") {
		t.Fatalf("DEM TileJSON URL = %#v, want a .png tile URL", tilesField[0])
	}
}

func TestRasterDEMEncodingOverrideDeclaresUnlabelledTerrain(t *testing.T) {
	// A DEM source that records neither a terrain format nor an encoding row
	// is indistinguishable from a plain raster until the operator says so.
	dataset := testDEMDataset(t, "png", "")
	plain, err := New(Config{Dataset: dataset, DatasetID: "dem", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, present := fetchTileJSON(t, plain)["encoding"]; present {
		t.Fatal("undeclared raster tileset advertised an elevation encoding")
	}

	declared, err := New(Config{Dataset: dataset, DatasetID: "dem", PublicBase: "https://tiles.example", DEMEncoding: "mapbox"})
	if err != nil {
		t.Fatal(err)
	}
	defer declared.Close()
	if got := fetchTileJSON(t, declared)["encoding"]; got != "mapbox" {
		t.Fatalf("declared TileJSON encoding = %#v, want mapbox", got)
	}

	if _, err := New(Config{Dataset: dataset, DatasetID: "dem", DEMEncoding: "nonsense"}); err == nil {
		t.Fatal("server accepted an unknown DEM encoding")
	}
}

func TestTileJSONOmitsVectorLayersWhenJSONMetadataIsMalformed(t *testing.T) {
	// testServer's fixture is a vector (pbf) dataset with no "json" metadata
	// row at all, which is also how a malformed/absent value must behave:
	// tileJSON must still succeed and simply omit the optional field.
	server := testServer(t)
	defer server.Close()

	payload := fetchTileJSON(t, server)
	if _, present := payload["vector_layers"]; present {
		t.Fatalf("TileJSON unexpectedly carries vector_layers without source metadata: %#v", payload["vector_layers"])
	}
}
