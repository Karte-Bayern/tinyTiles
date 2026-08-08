//go:build sqliteimport

package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	tinytiles "github.com/Karte-Bayern/tinyTiles/v2"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// testDatasetWithTile builds a fresh one-tile fixture at z2/x1/y2 (TMS) with
// the given payload, mirroring testDataset but with caller-chosen bytes so a
// swap test can tell the pre- and post-swap dataset apart.
func testDatasetWithTile(t *testing.T, payload []byte) *tinytiles.Dataset {
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
			('name', 'fixture'), ('format', 'pbf'), ('minzoom', '2'), ('maxzoom', '2');
		INSERT INTO tiles VALUES (2, 1, 2, ?);`, payload)
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
	return dataset
}

func fetchXYZ(t *testing.T, server *Server) (int, []byte, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://tiles.example/tiles/2/1/1.mvt", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response.Code, response.Body.Bytes(), response.Header().Get("ETag")
}

func TestSwapDatasetServesNewGenerationWithoutDowntime(t *testing.T) {
	before := testDatasetWithTile(t, []byte{1, 2, 3})
	server, err := New(Config{Dataset: before, DatasetID: "fixture", PublicBase: "https://tiles.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if code, body, _ := fetchXYZ(t, server); code != http.StatusOK || string(body) != string([]byte{1, 2, 3}) {
		t.Fatalf("pre-swap response = %d %q", code, body)
	}
	beforeCode, _, beforeETag := fetchXYZ(t, server)
	if beforeCode != http.StatusOK {
		t.Fatalf("pre-swap status = %d", beforeCode)
	}

	after := testDatasetWithTile(t, []byte{9, 8, 7})
	previous, err := server.SwapDataset(after)
	if err != nil {
		t.Fatalf("SwapDataset: %v", err)
	}
	if previous != before {
		t.Fatalf("SwapDataset returned previous dataset = %v, want the pre-swap dataset", previous)
	}

	if code, body, etag := fetchXYZ(t, server); code != http.StatusOK || string(body) != string([]byte{9, 8, 7}) {
		t.Fatalf("post-swap response = %d %q", code, body)
	} else if etag == beforeETag {
		t.Fatalf("post-swap ETag did not change: %q", etag)
	}

	// Dataset.Close blocks until its own in-flight lookups finish, so closing
	// the dataset SwapDataset just retired is safe immediately afterward.
	if err := previous.Close(); err != nil {
		t.Fatalf("close retired dataset: %v", err)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close current dataset: %v", err)
	}
}

func TestSwapDatasetRejectsInvalidDatasetAndKeepsServing(t *testing.T) {
	current := testDatasetWithTile(t, []byte{1, 2, 3})
	server, err := New(Config{Dataset: current, DatasetID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer func() { _ = current.Close() }()

	if _, err := server.SwapDataset(nil); err == nil {
		t.Fatal("SwapDataset(nil) accepted")
	}

	closed := testDatasetWithTile(t, []byte{9, 8, 7})
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.SwapDataset(closed); err == nil {
		t.Fatal("SwapDataset accepted an already-closed dataset")
	}

	if code, body, _ := fetchXYZ(t, server); code != http.StatusOK || string(body) != string([]byte{1, 2, 3}) {
		t.Fatalf("server switched away from its valid generation after a rejected swap: %d %q", code, body)
	}
}

func TestSwapDatasetConcurrentWithRequests(t *testing.T) {
	current := testDatasetWithTile(t, []byte{1, 2, 3})
	server, err := New(Config{Dataset: current, DatasetID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	next := testDatasetWithTile(t, []byte{9, 8, 7})
	defer func() { _ = next.Close() }()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				code, body, _ := fetchXYZ(t, server)
				if code != http.StatusOK {
					t.Errorf("concurrent request status = %d", code)
					return
				}
				if string(body) != string([]byte{1, 2, 3}) && string(body) != string([]byte{9, 8, 7}) {
					t.Errorf("concurrent request body = %x, want pre- or post-swap payload", body)
					return
				}
			}
		}()
	}

	previous, err := server.SwapDataset(next)
	if err != nil {
		t.Fatalf("SwapDataset: %v", err)
	}
	close(stop)
	wg.Wait()

	if err := previous.Close(); err != nil {
		t.Fatalf("close retired dataset: %v", err)
	}
}
