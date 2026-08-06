//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// compactMBTilesStats describes the lossless payload deduplication performed
// before a compact artifact import. PayloadBytes intentionally excludes the
// normalized map and image indexes: it is the part whose saving is guaranteed
// by equal tile byte streams.
type compactMBTilesStats struct {
	SourceMBTilesBytes  int64
	StagingMBTilesBytes int64
	Tiles               int64
	UniquePayloads      int64
	PayloadBytes        int64
	UniquePayloadBytes  int64
}

// ReusedTiles is the number of coordinate rows that reused an earlier,
// byte-identical payload. It deliberately distinguishes content reuse from
// the number of unique coordinates in the source.
func (s compactMBTilesStats) ReusedTiles() int64 { return s.Tiles - s.UniquePayloads }

func (s compactMBTilesStats) DuplicatePayloadBytes() int64 {
	return s.PayloadBytes - s.UniquePayloadBytes
}

// compactMBTilesSource is a private, temporary normalized MBTiles source. It
// must be removed after ImportMBTiles has consumed it.
type compactMBTilesSource struct {
	Path    string
	Stats   compactMBTilesStats
	cleanup func()
}

func (s compactMBTilesSource) Close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

const compactCommitBatch = 4_096

// compactMBTiles builds a temporary normalized MBTiles source whose map rows
// refer to one content-addressed image row per distinct payload. The final
// image IDs are short, deterministic base-36 sequence numbers; SHA-256 is
// retained only in the temporary hash lookup table. A byte comparison protects
// correctness even if two different payloads share a hash.
//
// The staging file deliberately uses the source basename. tinySQL records the
// basename in manifest.Source, so a compact import retains the normal source
// identity rather than exposing an implementation-specific temporary name.
func compactMBTiles(ctx context.Context, source, artifact string) (compactMBTilesSource, error) {
	if err := ctx.Err(); err != nil {
		return compactMBTilesSource{}, err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return compactMBTilesSource{}, fmt.Errorf("stat compact MBTiles source: %w", err)
	}
	parent := filepath.Dir(filepath.Clean(artifact))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return compactMBTilesSource{}, fmt.Errorf("create compact MBTiles parent: %w", err)
	}
	work, err := os.MkdirTemp(parent, ".tinytiles-compact-*")
	if err != nil {
		return compactMBTilesSource{}, fmt.Errorf("create compact MBTiles workspace: %w", err)
	}
	result := compactMBTilesSource{
		Path:  filepath.Join(work, filepath.Base(filepath.Clean(source))),
		Stats: compactMBTilesStats{SourceMBTilesBytes: sourceInfo.Size()},
		cleanup: func() {
			_ = os.RemoveAll(work)
		},
	}
	keep := false
	defer func() {
		if !keep {
			result.Close()
		}
	}()

	sourceDB, err := sql.Open("sqlite", compactReadOnlyURI(source))
	if err != nil {
		return compactMBTilesSource{}, fmt.Errorf("open compact MBTiles source: %w", err)
	}
	defer sourceDB.Close()
	sourceDB.SetMaxOpenConns(1)
	sourceDB.SetMaxIdleConns(1)
	if err := sourceDB.PingContext(ctx); err != nil {
		return compactMBTilesSource{}, fmt.Errorf("ping compact MBTiles source: %w", err)
	}

	targetDB, err := sql.Open("sqlite", result.Path)
	if err != nil {
		return compactMBTilesSource{}, fmt.Errorf("create compact MBTiles staging source: %w", err)
	}
	targetOpen := true
	defer func() {
		if targetOpen {
			_ = targetDB.Close()
		}
	}()
	targetDB.SetMaxOpenConns(1)
	targetDB.SetMaxIdleConns(1)
	if err := createCompactMBTilesSchema(ctx, targetDB); err != nil {
		return compactMBTilesSource{}, err
	}
	if err := copyCompactMetadata(ctx, sourceDB, targetDB); err != nil {
		return compactMBTilesSource{}, err
	}
	stats, err := copyCompactTiles(ctx, sourceDB, targetDB)
	if err != nil {
		return compactMBTilesSource{}, err
	}
	if _, err := targetDB.ExecContext(ctx, `CREATE INDEX map_zxy ON map(zoom_level, tile_column, tile_row)`); err != nil {
		return compactMBTilesSource{}, fmt.Errorf("index compact MBTiles map: %w", err)
	}
	if err := targetDB.Close(); err != nil {
		return compactMBTilesSource{}, fmt.Errorf("close compact MBTiles staging source: %w", err)
	}
	targetOpen = false
	stagingInfo, err := os.Stat(result.Path)
	if err != nil {
		return compactMBTilesSource{}, fmt.Errorf("stat compact MBTiles staging source: %w", err)
	}
	stats.SourceMBTilesBytes = sourceInfo.Size()
	stats.StagingMBTilesBytes = stagingInfo.Size()
	result.Stats = stats
	keep = true
	return result, nil
}

func compactReadOnlyURI(path string) string {
	u := &url.URL{Scheme: "file", Path: filepath.Clean(path)}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	u.RawQuery = query.Encode()
	return u.String()
}

func createCompactMBTilesSchema(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{
		`PRAGMA journal_mode=OFF`,
		`PRAGMA synchronous=OFF`,
		`PRAGMA temp_store=MEMORY`,
		// Keep SQLite's temporary staging cache bounded independently from the
		// tinySQL importer cache that follows it.
		`PRAGMA cache_size=-16384`,
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE map (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_id TEXT)`,
		`CREATE TABLE images (tile_id TEXT PRIMARY KEY, tile_data BLOB)`,
		`CREATE TABLE hashes (hash BLOB NOT NULL, tile_id TEXT NOT NULL, PRIMARY KEY(hash, tile_id))`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize compact MBTiles staging source: %w", err)
		}
	}
	return nil
}

func copyCompactMetadata(ctx context.Context, source, target *sql.DB) error {
	exists, err := sqliteTableExistsContext(ctx, source, "metadata")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	rows, err := source.QueryContext(ctx, `SELECT name, value FROM metadata ORDER BY name`)
	if err != nil {
		return fmt.Errorf("read MBTiles metadata for compaction: %w", err)
	}
	defer rows.Close()
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compact metadata copy: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO metadata(name, value) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare compact metadata copy: %w", err)
	}
	defer insert.Close()
	for rows.Next() {
		var name, value sql.NullString
		if err := rows.Scan(&name, &value); err != nil {
			return fmt.Errorf("scan MBTiles metadata for compaction: %w", err)
		}
		if !name.Valid || !value.Valid {
			return errors.New("MBTiles metadata contains NULL name/value")
		}
		if _, err := insert.ExecContext(ctx, name.String, value.String); err != nil {
			return fmt.Errorf("write compact metadata: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read MBTiles metadata for compaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit compact metadata copy: %w", err)
	}
	committed = true
	return nil
}

func copyCompactTiles(ctx context.Context, source, target *sql.DB) (compactMBTilesStats, error) {
	flat, err := sqliteTableExistsContext(ctx, source, "tiles")
	if err != nil {
		return compactMBTilesStats{}, err
	}
	if flat {
		return copyCompactFlatTiles(ctx, source, target)
	}
	mapTable, err := sqliteTableExistsContext(ctx, source, "map")
	if err != nil {
		return compactMBTilesStats{}, err
	}
	images, err := sqliteTableExistsContext(ctx, source, "images")
	if err != nil {
		return compactMBTilesStats{}, err
	}
	if !mapTable || !images {
		return compactMBTilesStats{}, errors.New("MBTiles source has neither tiles nor map/images schema")
	}
	return copyCompactNormalizedTiles(ctx, source, target)
}

func copyCompactFlatTiles(ctx context.Context, source, target *sql.DB) (compactMBTilesStats, error) {
	rows, err := source.QueryContext(ctx, `SELECT zoom_level, tile_column, tile_row, tile_data FROM tiles ORDER BY zoom_level, tile_column, tile_row`)
	if err != nil {
		return compactMBTilesStats{}, fmt.Errorf("query flat MBTiles for compaction: %w", err)
	}
	defer rows.Close()
	writer, err := newCompactTileWriter(ctx, target, compactCommitBatch)
	if err != nil {
		return compactMBTilesStats{}, err
	}
	defer writer.abort()
	for rows.Next() {
		var z, x, y int
		var data []byte
		if err := rows.Scan(&z, &x, &y, &data); err != nil {
			return compactMBTilesStats{}, fmt.Errorf("scan flat MBTiles tile for compaction: %w", err)
		}
		if err := writer.add(ctx, z, x, y, data); err != nil {
			return compactMBTilesStats{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return compactMBTilesStats{}, fmt.Errorf("query flat MBTiles for compaction: %w", err)
	}
	if err := writer.finish(); err != nil {
		return compactMBTilesStats{}, err
	}
	return writer.stats, nil
}

func copyCompactNormalizedTiles(ctx context.Context, source, target *sql.DB) (compactMBTilesStats, error) {
	// LEFT JOIN makes a dangling map reference an explicit error rather than
	// silently dropping it during staging.
	rows, err := source.QueryContext(ctx, `
		SELECT m.zoom_level, m.tile_column, m.tile_row, m.tile_id, i.tile_id, i.tile_data
		FROM map AS m LEFT JOIN images AS i ON i.tile_id = m.tile_id
		ORDER BY m.zoom_level, m.tile_column, m.tile_row`)
	if err != nil {
		return compactMBTilesStats{}, fmt.Errorf("query normalized MBTiles for compaction: %w", err)
	}
	defer rows.Close()
	writer, err := newCompactTileWriter(ctx, target, compactCommitBatch)
	if err != nil {
		return compactMBTilesStats{}, err
	}
	defer writer.abort()
	for rows.Next() {
		var z, x, y int
		var mapID string
		var imageID sql.NullString
		var data []byte
		if err := rows.Scan(&z, &x, &y, &mapID, &imageID, &data); err != nil {
			return compactMBTilesStats{}, fmt.Errorf("scan normalized MBTiles tile for compaction: %w", err)
		}
		if !imageID.Valid {
			return compactMBTilesStats{}, fmt.Errorf("normalized MBTiles map tile_id %q has no image", mapID)
		}
		if err := writer.add(ctx, z, x, y, data); err != nil {
			return compactMBTilesStats{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return compactMBTilesStats{}, fmt.Errorf("query normalized MBTiles for compaction: %w", err)
	}
	if err := writer.finish(); err != nil {
		return compactMBTilesStats{}, err
	}
	return writer.stats, nil
}

// compactTileWriter persists a bounded number of map/image/hash changes per
// SQLite transaction. It does not retain the source payload corpus in memory.
type compactTileWriter struct {
	db          *sql.DB
	tx          *sql.Tx
	mapInsert   *sql.Stmt
	imageInsert *sql.Stmt
	hashInsert  *sql.Stmt
	hashLookup  *sql.Stmt
	batchSize   int
	written     int
	nextID      int64
	stats       compactMBTilesStats
	hash        func([]byte) [sha256.Size]byte
}

func newCompactTileWriter(ctx context.Context, db *sql.DB, batchSize int) (*compactTileWriter, error) {
	if batchSize < 1 {
		batchSize = compactCommitBatch
	}
	writer := &compactTileWriter{db: db, batchSize: batchSize, hash: sha256.Sum256}
	if err := writer.begin(ctx); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *compactTileWriter) begin(ctx context.Context) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compact tile batch: %w", err)
	}
	mapInsert, err := tx.PrepareContext(ctx, `INSERT INTO map(zoom_level, tile_column, tile_row, tile_id) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare compact map write: %w", err)
	}
	imageInsert, err := tx.PrepareContext(ctx, `INSERT INTO images(tile_id, tile_data) VALUES (?, ?)`)
	if err != nil {
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return fmt.Errorf("prepare compact image write: %w", err)
	}
	hashInsert, err := tx.PrepareContext(ctx, `INSERT INTO hashes(hash, tile_id) VALUES (?, ?)`)
	if err != nil {
		_ = imageInsert.Close()
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return fmt.Errorf("prepare compact hash write: %w", err)
	}
	hashLookup, err := tx.PrepareContext(ctx, `
		SELECT i.tile_id, i.tile_data
		FROM hashes AS h JOIN images AS i ON i.tile_id = h.tile_id
		WHERE h.hash = ?
		ORDER BY h.tile_id`)
	if err != nil {
		_ = hashInsert.Close()
		_ = imageInsert.Close()
		_ = mapInsert.Close()
		_ = tx.Rollback()
		return fmt.Errorf("prepare compact hash lookup: %w", err)
	}
	w.tx = tx
	w.mapInsert = mapInsert
	w.imageInsert = imageInsert
	w.hashInsert = hashInsert
	w.hashLookup = hashLookup
	w.written = 0
	return nil
}

func (w *compactTileWriter) add(ctx context.Context, z, x, y int, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, unique, err := w.imageID(ctx, data)
	if err != nil {
		return err
	}
	if _, err := w.mapInsert.ExecContext(ctx, z, x, y, id); err != nil {
		return fmt.Errorf("write compact map tile z=%d x=%d y=%d: %w", z, x, y, err)
	}
	w.stats.Tiles++
	w.stats.PayloadBytes += int64(len(data))
	if unique {
		w.stats.UniquePayloads++
		w.stats.UniquePayloadBytes += int64(len(data))
	}
	w.written++
	if w.written < w.batchSize {
		return nil
	}
	if err := w.commit(); err != nil {
		return err
	}
	return w.begin(ctx)
}

func (w *compactTileWriter) imageID(ctx context.Context, data []byte) (string, bool, error) {
	sum := w.hash(data)
	rows, err := w.hashLookup.QueryContext(ctx, sum[:])
	if err != nil {
		return "", false, fmt.Errorf("look up compact tile hash: %w", err)
	}
	for rows.Next() {
		var id string
		var existing []byte
		if err := rows.Scan(&id, &existing); err != nil {
			_ = rows.Close()
			return "", false, fmt.Errorf("scan compact tile hash: %w", err)
		}
		if bytes.Equal(existing, data) {
			if err := rows.Close(); err != nil {
				return "", false, fmt.Errorf("close compact tile hash lookup: %w", err)
			}
			return id, false, nil
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", false, fmt.Errorf("look up compact tile hash: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", false, fmt.Errorf("close compact tile hash lookup: %w", err)
	}
	if w.nextID < 0 {
		return "", false, errors.New("too many unique tiles for compact import")
	}
	id := strconv.FormatInt(w.nextID, 36)
	w.nextID++
	if _, err := w.imageInsert.ExecContext(ctx, id, data); err != nil {
		return "", false, fmt.Errorf("write compact tile image: %w", err)
	}
	if _, err := w.hashInsert.ExecContext(ctx, sum[:], id); err != nil {
		return "", false, fmt.Errorf("write compact tile hash: %w", err)
	}
	return id, true, nil
}

func (w *compactTileWriter) finish() error {
	if w.tx == nil {
		return nil
	}
	return w.commit()
}

func (w *compactTileWriter) commit() error {
	w.closeStatements()
	err := w.tx.Commit()
	w.tx = nil
	if err != nil {
		return fmt.Errorf("commit compact tile batch: %w", err)
	}
	return nil
}

func (w *compactTileWriter) abort() {
	if w.tx == nil {
		return
	}
	w.closeStatements()
	_ = w.tx.Rollback()
	w.tx = nil
}

func (w *compactTileWriter) closeStatements() {
	if w.hashLookup != nil {
		_ = w.hashLookup.Close()
		w.hashLookup = nil
	}
	if w.hashInsert != nil {
		_ = w.hashInsert.Close()
		w.hashInsert = nil
	}
	if w.imageInsert != nil {
		_ = w.imageInsert.Close()
		w.imageInsert = nil
	}
	if w.mapInsert != nil {
		_ = w.mapInsert.Close()
		w.mapInsert = nil
	}
}

func compactImportProvenance(provenance map[string]any, source string, stats compactMBTilesStats) map[string]any {
	out := make(map[string]any, len(provenance)+1)
	for key, value := range provenance {
		out[key] = value
	}
	out["tinytiles_compaction"] = map[string]any{
		"mode":                    "lossless-content-deduplication",
		"source":                  filepath.Base(filepath.Clean(source)),
		"source_mbtiles_bytes":    stats.SourceMBTilesBytes,
		"staging_mbtiles_bytes":   stats.StagingMBTilesBytes,
		"tiles":                   stats.Tiles,
		"unique_payloads":         stats.UniquePayloads,
		"payload_bytes":           stats.PayloadBytes,
		"unique_payload_bytes":    stats.UniquePayloadBytes,
		"duplicate_payload_bytes": stats.DuplicatePayloadBytes(),
		"hash":                    "sha256-with-bytewise-collision-check",
		"tile_id":                 "base36-sequential",
	}
	return out
}

func compactStatsLine(stats compactMBTilesStats) string {
	return fmt.Sprintf(
		"compact tiles=%d unique-payloads=%d reused-tiles=%d payload=%dB unique-payload=%dB deduplicated-payload=%dB staging=%dB",
		stats.Tiles,
		stats.UniquePayloads,
		stats.ReusedTiles(),
		stats.PayloadBytes,
		stats.UniquePayloadBytes,
		stats.DuplicatePayloadBytes(),
		stats.StagingMBTilesBytes,
	)
}

func artifactDirectoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure artifact bytes: %w", err)
	}
	return total, nil
}
