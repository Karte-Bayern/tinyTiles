//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// BenchmarkImportArtifactBatch measures the full, validated MBTiles-to-ttiles
// publication path. The fixture deliberately holds enough tiles that append
// checkpoint frequency is observable, while keeping the benchmark practical
// for a regular developer machine.
func BenchmarkImportArtifactBatch(b *testing.B) {
	source := filepath.Join(b.TempDir(), "source.mbtiles")
	if err := createImportBenchmarkMBTiles(source, 32_768, 512); err != nil {
		b.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		batch int
	}{
		{name: "auto", batch: 0},
		{name: "batch=1000", batch: 1_000},
		{name: "batch=2048", batch: 2_048},
		{name: "batch=4096", batch: 4_096},
		{name: "batch=8192", batch: 8_192},
		{name: "batch=16384", batch: 16_384},
	} {
		b.Run(tc.name, func(b *testing.B) {
			root := b.TempDir()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				artifact := filepath.Join(root, fmt.Sprintf("%d.ttiles", i))
				if _, err := importArtifact(context.Background(), source, artifact, tiles.SchemaFlat, tc.batch, 256<<20, 0, false, io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func createImportBenchmarkMBTiles(path string, count, tileBytes int) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata(name, value) VALUES ('format', 'pbf'), ('name', 'import benchmark');
	`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO tiles(zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	payload := make([]byte, tileBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	for i := 0; i < count; i++ {
		payload[0] = byte(i)
		if _, err := stmt.Exec(15, i%256, i/256, payload); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
