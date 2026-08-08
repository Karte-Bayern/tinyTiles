//go:build sqliteimport

package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	tinytiles "github.com/Karte-Bayern/tinyTiles/v2"
	_ "github.com/SimonWaldherr/tinySQL/importer"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func BenchmarkXYZHandlerWarmTile(b *testing.B) {
	const tileSize = 64 << 10
	for _, scenario := range []struct {
		name       string
		cacheBytes int64
		warm       bool
	}{
		{name: "without_cache", cacheBytes: -1},
		// The payload cannot fit in this cache, but its checksum can. It
		// models a revisited tile after payload eviction or a very large tile.
		{name: "checksum_cache", cacheBytes: tileCacheShardCount * 4 << 10, warm: true},
		{name: "with_cache", cacheBytes: DefaultTileCacheBytes, warm: true},
	} {
		scenario := scenario
		name := scenario.name
		b.Run(name, func(b *testing.B) {
			server := benchmarkServer(b, tileSize, scenario.cacheBytes)
			handler := server.XYZHandler()
			request := httptest.NewRequest(http.MethodGet, "https://tiles.example/2/1/1.mvt", nil)
			if scenario.warm {
				handler.ServeHTTP(&benchmarkResponseWriter{header: make(http.Header)}, request)
			}
			b.SetBytes(tileSize)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				writer := &benchmarkResponseWriter{header: make(http.Header)}
				handler.ServeHTTP(writer, request)
				if writer.status != http.StatusOK {
					b.Fatalf("status = %d", writer.status)
				}
			}
		})
	}
}

// BenchmarkXYZHandlerWarmTileParallel measures the common map-pan case where
// multiple HTTP clients request one already-cached tile at once. The request
// is immutable during ServeHTTP, just as net/http hands each handler an
// independently owned read-only request.
func BenchmarkXYZHandlerWarmTileParallel(b *testing.B) {
	const tileSize = 64 << 10
	server := benchmarkServer(b, tileSize, DefaultTileCacheBytes)
	handler := server.XYZHandler()
	request := httptest.NewRequest(http.MethodGet, "https://tiles.example/2/1/1.mvt", nil)
	handler.ServeHTTP(&benchmarkResponseWriter{header: make(http.Header)}, request)
	b.SetBytes(tileSize)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			writer := &benchmarkResponseWriter{header: make(http.Header)}
			handler.ServeHTTP(writer, request)
			if writer.status != http.StatusOK {
				b.Fatalf("status = %d", writer.status)
			}
		}
	})
}

type benchmarkResponseWriter struct {
	header http.Header
	status int
}

func (w *benchmarkResponseWriter) Header() http.Header { return w.header }

func (w *benchmarkResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(data), nil
}

func (w *benchmarkResponseWriter) WriteHeader(status int) { w.status = status }

func benchmarkServer(t testing.TB, tileSize int, cacheBytes int64) *Server {
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
		INSERT INTO metadata VALUES ('name', 'benchmark'), ('format', 'pbf');`)
	if err == nil {
		payload := make([]byte, tileSize)
		for index := range payload {
			payload[index] = byte(index)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO tiles VALUES (2, 1, 2, ?)`, payload)
	}
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
	dataset, err := tinytiles.Open(context.Background(), artifact, tinytiles.OpenOptions{Readers: 1, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataset.Close() })
	server, err := New(Config{Dataset: dataset, DatasetID: "benchmark", PublicBase: "https://tiles.example", TileCacheBytes: cacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
