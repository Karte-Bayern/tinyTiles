//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestCLIArtifactLifecycle(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "region.ttiles")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--batch", "2", "--min-free", "0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "preflight tiles=12") || !strings.Contains(stdout.String(), "published tiles=12") {
		t.Fatalf("import progress missing from stdout=%q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"validate", artifact}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "valid schema=flat") {
		t.Fatalf("validate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"inspect", artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var manifest tiles.ArtifactInfo
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("inspect JSON: %v\n%s", err, stdout.String())
	}
	if manifest.Schema != tiles.SchemaFlat || len(manifest.Tables) != 2 {
		t.Fatalf("inspect manifest schema=%q tables=%d", manifest.Schema, len(manifest.Tables))
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"tile", artifact, "8", "2", "17"}, &stdout, &stderr); code != 0 || !bytes.Equal(stdout.Bytes(), []byte{1, 2, 17}) {
		t.Fatalf("tile code=%d data=%x stderr=%q", code, stdout.Bytes(), stderr.String())
	}
	tileFile := filepath.Join(temp, "tile.pbf")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"tile", "-out", tileFile, artifact, "8", "2", "17"}, &stdout, &stderr); code != 0 {
		t.Fatalf("tile -out code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(tileFile)
	if err != nil || !bytes.Equal(data, []byte{1, 2, 17}) {
		t.Fatalf("tile output=%x err=%v", data, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"tile", artifact, "8", "200", "200"}, &stdout, &stderr); code != 3 {
		t.Fatalf("missing tile code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// Exercise close/reopen behaviour through the public reader API as a CLI
	// artifact is expected to survive independent serving process lifecycles.
	for cycle := 0; cycle < 3; cycle++ {
		reader, err := tiles.OpenArtifact(context.Background(), artifact, tiles.OpenOptions{MaxMemoryBytes: 8 << 20})
		if err != nil {
			t.Fatalf("open reader cycle %d: %v", cycle, err)
		}
		tile, found, err := reader.Lookup(context.Background(), tiles.Key{Z: 8, X: 2, Y: 17})
		closeErr := reader.Close()
		if err != nil || closeErr != nil || !found || !bytes.Equal(tile.Data, []byte{1, 2, 17}) {
			t.Fatalf("reader cycle %d found=%t data=%x lookup=%v close=%v", cycle, found, tile.Data, err, closeErr)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"benchmark", "--source", source, "--artifact", artifact, "--requests", "10", "--cold-runs", "3", "--cold-max-p95-ratio", "1000", "--cold-request", "--cold-request-max-p95-ratio", "1000"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "full-parity\ttiles=") || !strings.Contains(stdout.String(), "cold-mode\tfresh Dataset readers and SQLite statements per profile; randomized unique corpus; OS filesystem cache not forcibly evicted") || !strings.Contains(stdout.String(), "cold-request-mode\tone fresh SQLite connection or Dataset per requested tile; artifact validated once before timing; randomized unique corpus; OS filesystem cache not forcibly evicted") || !strings.Contains(stdout.String(), "runs=3 percentile-aggregation=median") || !strings.Contains(stdout.String(), "cold-aggregation\tmedian of per-run p50/p95/p99 across 3 complete fresh-reader runs; SQLite/tinyTiles measurement order alternates; gate uses median p95") || !strings.Contains(stdout.String(), "cold-request-aggregation\tmedian of per-run p50/p95/p99 across 3 complete application-cold runs; backend order alternates for every request; gate uses lookup p95") || !strings.Contains(stdout.String(), "application-cold-request\tSQLite") || !strings.Contains(stdout.String(), "application-cold-request\ttinyTiles") || !strings.Contains(stdout.String(), "fresh-reader-corpus\tSQLite\t1") || !strings.Contains(stdout.String(), "fresh-reader-corpus\ttinyTiles\t8") || !strings.Contains(stdout.String(), "schema\tflat\tflat") || !strings.Contains(stdout.String(), "cold-gate\tmedian-p95 <= 1000.000x SQLite\tPASS") || !strings.Contains(stdout.String(), "cold-request-gate\tmedian-lookup-p95 <= 1000.000x SQLite\tPASS") || !strings.Contains(stdout.String(), "gate\tp95 <= 2x SQLite\tPASS") {
		t.Fatalf("benchmark code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// Full parity includes metadata, not only the sampled tile corpus.
	sourceDB, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceDB.Exec(`UPDATE metadata SET value='changed after import' WHERE name='name'`); err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"benchmark", "--source", source, "--artifact", artifact, "--requests", "10"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "full parity") {
		t.Fatalf("changed source benchmark code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCommandValidationErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"benchmark", "--requests", "9"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid benchmark code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"benchmark", "--source", "source.mbtiles", "--artifact", "artifact.ttiles", "--cold-max-p95-ratio", "-1"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid cold gate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"benchmark", "--source", "source.mbtiles", "--artifact", "artifact.ttiles", "--cold-request-max-p95-ratio", "-1"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid application-cold gate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"benchmark", "--source", "source.mbtiles", "--artifact", "artifact.ttiles", "--cold-runs", "0"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid cold runs code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"tile", "dataset.ttiles", "8", "x", "1"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid tile coordinate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCommandBenchmarkSupportsNormalizedSource(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "normalized.mbtiles")
	if err := createNormalizedMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "normalized.ttiles")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--min-free", "0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "schema-resolution requested=auto resolved=flat map=12 unique-tile-ids=12") {
		t.Fatalf("auto schema resolution missing from stdout=%q", stdout.String())
	}
	manifest, err := tiles.ValidateArtifact(context.Background(), artifact)
	if err != nil {
		t.Fatalf("validate auto-resolved artifact: %v", err)
	}
	if manifest.Schema != tiles.SchemaFlat {
		t.Fatalf("auto schema=%q, want flat for a normalized source without tile_id reuse", manifest.Schema)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"benchmark", "--source", source, "--artifact", artifact, "--requests", "10"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "full-parity\ttiles=") || !strings.Contains(stdout.String(), "gate\tp95 <= 2x SQLite\tPASS") {
		t.Fatalf("benchmark code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCommandImportAutoSchemaKeepsNormalizedTileReuse(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "normalized-reuse.mbtiles")
	if err := createNormalizedMBTilesReuseFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "normalized-reuse.ttiles")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--min-free", "0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "schema-resolution requested=auto resolved=auto map=12 unique-tile-ids=11") {
		t.Fatalf("reuse schema resolution missing from stdout=%q", stdout.String())
	}
	manifest, err := tiles.ValidateArtifact(context.Background(), artifact)
	if err != nil {
		t.Fatalf("validate auto-resolved reuse artifact: %v", err)
	}
	if manifest.Schema != tiles.SchemaNormalized {
		t.Fatalf("auto schema=%q, want normalized when tile_id is reused", manifest.Schema)
	}
}

func TestBenchmarkZoomQuotasRedistributeSparseZooms(t *testing.T) {
	got := benchmarkZoomQuotas([]int{1, 2, 100}, 10)
	want := []int{1, 2, 7}
	if len(got) != len(want) {
		t.Fatalf("quota length=%d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("quota[%d]=%d, want %d (all quotas=%v)", index, got[index], want[index], got)
		}
	}
}

func createFlatMBTilesFixture(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata(name, value) VALUES ('format', 'pbf'), ('name', 'flat fixture');
	`); err != nil {
		return err
	}
	statement, err := db.Prepare(`INSERT INTO tiles(zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for x := 1; x <= 12; x++ {
		if _, err := statement.Exec(8, x, x+15, []byte{1, byte(x), byte(x + 15)}); err != nil {
			return err
		}
	}
	return nil
}

func createNormalizedMBTilesFixture(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE map (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_id TEXT);
		CREATE TABLE images (tile_id TEXT PRIMARY KEY, tile_data BLOB);
		INSERT INTO metadata(name, value) VALUES ('format', 'pbf'), ('name', 'normalized fixture');
	`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	mapInsert, err := tx.Prepare(`INSERT INTO map(zoom_level,tile_column,tile_row,tile_id) VALUES(?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	imageInsert, err := tx.Prepare(`INSERT INTO images(tile_id,tile_data) VALUES(?,?)`)
	if err != nil {
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return err
	}
	for x := 1; x <= 12; x++ {
		id := fmt.Sprintf("tile-%d", x)
		if _, err := imageInsert.Exec(id, []byte{2, byte(x), byte(x + 15)}); err != nil {
			_ = imageInsert.Close()
			_ = mapInsert.Close()
			_ = tx.Rollback()
			return err
		}
		if _, err := mapInsert.Exec(8, x, x+15, id); err != nil {
			_ = imageInsert.Close()
			_ = mapInsert.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := imageInsert.Close(); err != nil {
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return err
	}
	if err := mapInsert.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func createNormalizedMBTilesReuseFixture(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE map (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_id TEXT);
		CREATE TABLE images (tile_id TEXT PRIMARY KEY, tile_data BLOB);
		INSERT INTO metadata(name, value) VALUES ('format', 'pbf'), ('name', 'normalized reuse fixture');
	`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	mapInsert, err := tx.Prepare(`INSERT INTO map(zoom_level,tile_column,tile_row,tile_id) VALUES(?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	imageInsert, err := tx.Prepare(`INSERT INTO images(tile_id,tile_data) VALUES(?,?)`)
	if err != nil {
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return err
	}
	for x := 1; x <= 11; x++ {
		id := fmt.Sprintf("tile-%d", x)
		if _, err := imageInsert.Exec(id, []byte{3, byte(x), byte(x + 15)}); err != nil {
			_ = imageInsert.Close()
			_ = mapInsert.Close()
			_ = tx.Rollback()
			return err
		}
		if _, err := mapInsert.Exec(8, x, x+15, id); err != nil {
			_ = imageInsert.Close()
			_ = mapInsert.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if _, err := mapInsert.Exec(8, 12, 27, "tile-1"); err != nil {
		_ = imageInsert.Close()
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return err
	}
	if err := imageInsert.Close(); err != nil {
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return err
	}
	if err := mapInsert.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
