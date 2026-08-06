//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestCommandImportCompactDeduplicatesAcrossCommitBoundary(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "repeated.mbtiles")
	const tileCount = compactCommitBatch + 1
	payloads := [][]byte{
		bytes.Repeat([]byte("water"), 512),
		bytes.Repeat([]byte("land"), 512),
		bytes.Repeat([]byte("transparent"), 256),
	}
	if err := createRepeatedFlatMBTiles(source, tileCount, payloads); err != nil {
		t.Fatal(err)
	}

	plain := filepath.Join(temp, "plain.ttiles")
	if _, err := importArtifact(context.Background(), source, plain, tiles.SchemaFlat, 512, 64<<20, 0, false, io.Discard); err != nil {
		t.Fatalf("plain import: %v", err)
	}
	plainBytes, err := artifactDirectoryBytes(plain)
	if err != nil {
		t.Fatal(err)
	}

	artifact := filepath.Join(temp, "compact.ttiles")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--compact", "--batch=512", "--min-free=0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("compact import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "phase=compact") || !strings.Contains(stdout.String(), "compact tiles=4097 unique-payloads=3 reused-tiles=4094") || !strings.Contains(stdout.String(), "compact artifact-bytes=") {
		t.Fatalf("compact progress missing: %q", stdout.String())
	}

	info, err := tiles.ValidateArtifact(context.Background(), artifact)
	if err != nil {
		t.Fatalf("validate compact artifact: %v", err)
	}
	if info.Schema != tiles.SchemaNormalized || info.Source != filepath.Base(source) {
		t.Fatalf("compact info schema=%q source=%q", info.Schema, info.Source)
	}
	if rows := artifactTableRows(info, "map"); rows != tileCount {
		t.Fatalf("compact map rows=%d, want %d", rows, tileCount)
	}
	if rows := artifactTableRows(info, "images"); rows != int64(len(payloads)) {
		t.Fatalf("compact image rows=%d, want %d", rows, len(payloads))
	}
	provenance, ok := info.Provenance["tinytiles_compaction"].(map[string]any)
	if !ok || provenance["tile_id"] != "base36-sequential" || provenance["hash"] != "sha256-with-bytewise-collision-check" {
		t.Fatalf("compact provenance=%#v", info.Provenance["tinytiles_compaction"])
	}

	reader, err := tiles.OpenArtifact(context.Background(), artifact, tiles.OpenOptions{MaxMemoryBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, x := range []int{0, compactCommitBatch - 1, compactCommitBatch} {
		tile, found, err := reader.Lookup(context.Background(), tiles.Key{Z: 13, X: x, Y: 1})
		if err != nil || !found || !bytes.Equal(tile.Data, payloads[x%len(payloads)]) {
			t.Fatalf("lookup x=%d found=%t data=%dB err=%v", x, found, len(tile.Data), err)
		}
	}
	compactBytes, err := artifactDirectoryBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if compactBytes >= plainBytes {
		t.Fatalf("compact artifact=%dB, plain artifact=%dB", compactBytes, plainBytes)
	}
	t.Logf("payload-deduplicated artifact: %d B vs %d B plain (%.1f%% smaller)", compactBytes, plainBytes, float64(plainBytes-compactBytes)*100/float64(plainBytes))
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tinytiles-compact-") {
			t.Fatalf("temporary compact workspace was not removed: %s", entry.Name())
		}
	}
}

func TestCommandImportCompactSupportsNormalizedSource(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "normalized.mbtiles")
	if err := createNormalizedMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "compact.ttiles")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--compact", "--min-free=0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("compact normalized import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	info, err := tiles.ValidateArtifact(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if info.Schema != tiles.SchemaNormalized || artifactTableRows(info, "map") != 12 || artifactTableRows(info, "images") != 12 {
		t.Fatalf("compact normalized info=%+v", info)
	}
	reader, err := tiles.OpenArtifact(context.Background(), artifact, tiles.OpenOptions{MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tile, found, err := reader.Lookup(context.Background(), tiles.Key{Z: 8, X: 2, Y: 17})
	if err != nil || !found || !bytes.Equal(tile.Data, []byte{2, 2, 17}) {
		t.Fatalf("compact normalized lookup found=%t data=%x err=%v", found, tile.Data, err)
	}
}

func TestCompactTileWriterKeepsDistinctPayloadsOnHashCollision(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "compact.mbtiles"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := createCompactMBTilesSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	writer, err := newCompactTileWriter(context.Background(), db, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.abort()
	writer.hash = func([]byte) [sha256.Size]byte { return [sha256.Size]byte{} }
	for x, payload := range [][]byte{[]byte("first"), []byte("second"), []byte("first")} {
		if err := writer.add(context.Background(), 2, x, 1, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if writer.stats.UniquePayloads != 2 || writer.stats.Tiles != 3 {
		t.Fatalf("collision stats=%+v", writer.stats)
	}
	var images, hashes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&images); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM hashes`).Scan(&hashes); err != nil {
		t.Fatal(err)
	}
	if images != 2 || hashes != 2 {
		t.Fatalf("collision rows images=%d hashes=%d", images, hashes)
	}
	rows, err := db.Query(`SELECT tile_column, tile_id FROM map ORDER BY tile_column`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var x int
		var id string
		if err := rows.Scan(&x, &id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(ids, ","), "0,1,0"; got != want {
		t.Fatalf("collision IDs=%q, want %q", got, want)
	}
}

func TestCommandImportCompactRejectsFlatSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--compact", "--schema=flat", "missing.mbtiles", "target.ttiles"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "schema auto or normalized") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func createRepeatedFlatMBTiles(path string, tiles int, payloads [][]byte) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata(name, value) VALUES ('format', 'pbf'), ('name', 'repeat fixture');
	`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	insert, err := tx.Prepare(`INSERT INTO tiles(zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for x := 0; x < tiles; x++ {
		if _, err := insert.Exec(13, x, 1, payloads[x%len(payloads)]); err != nil {
			_ = insert.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := insert.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func artifactTableRows(info tiles.ArtifactInfo, name string) int64 {
	for _, table := range info.Tables {
		if table.Name == name {
			return table.Rows
		}
	}
	return -1
}
