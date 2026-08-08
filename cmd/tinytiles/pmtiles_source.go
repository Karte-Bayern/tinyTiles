//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/pmtiles"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// pmtilesStagingStats describes a PMTiles to MBTiles staging pass.
type pmtilesStagingStats struct {
	SourcePMTilesBytes  int64
	StagingMBTilesBytes int64
	Tiles               int64
	TileBytes           int64
	MaxTileBytes        int64
	MinZoom             int
	MaxZoom             int
	Format              string
	ContentEncoding     string
}

// pmtilesTileSource exposes PMTiles directly through tinySQL's generic tile
// stream contract. No SQLite database or MBTiles staging file is created.
type pmtilesTileSource struct {
	path     string
	archive  *pmtiles.Archive
	metadata map[string]string
	stats    pmtilesStagingStats
}

func openPMTilesTileSource(ctx context.Context, path string) (*pmtilesTileSource, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat PMTiles source: %w", err)
	}
	archive, err := pmtiles.Open(path)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = archive.Close()
		}
	}()
	header := archive.Header()
	if encoding := header.TileCompression.ContentEncoding(); encoding != "" && encoding != "gzip" {
		return nil, fmt.Errorf("PMTiles archive stores tiles with %s compression; tinyTiles imports uncompressed or gzip tile payloads", header.TileCompression)
	}
	metadata, err := pmtilesMetadataRows(archive, header)
	if err != nil {
		return nil, err
	}
	streamStats, err := archive.InspectTiles(ctx)
	if err != nil {
		return nil, err
	}
	if streamStats.Tiles == 0 {
		return nil, fmt.Errorf("PMTiles archive %s contains no tiles", filepath.Base(path))
	}
	if streamStats.Tiles > math.MaxInt64 || streamStats.TileBytes > math.MaxInt64 || streamStats.MaxTileBytes > math.MaxInt64 {
		return nil, errors.New("PMTiles expanded tile stream exceeds supported size")
	}
	source := &pmtilesTileSource{
		path:     path,
		archive:  archive,
		metadata: metadata,
		stats: pmtilesStagingStats{
			SourcePMTilesBytes: info.Size(), Tiles: int64(streamStats.Tiles),
			TileBytes: int64(streamStats.TileBytes), MaxTileBytes: int64(streamStats.MaxTileBytes),
			MinZoom: int(header.MinZoom), MaxZoom: int(header.MaxZoom),
			Format: metadata["format"], ContentEncoding: header.TileCompression.ContentEncoding(),
		},
	}
	keep = true
	return source, nil
}

func (s *pmtilesTileSource) Close() error {
	if s == nil || s.archive == nil {
		return nil
	}
	err := s.archive.Close()
	s.archive = nil
	return err
}

func (s *pmtilesTileSource) Info(context.Context) (tiles.SourceInfo, error) {
	if s == nil || s.archive == nil {
		return tiles.SourceInfo{}, errors.New("PMTiles source is closed")
	}
	metadata := make(map[string]string, len(s.metadata))
	for name, value := range s.metadata {
		metadata[name] = value
	}
	return tiles.SourceInfo{
		Name: filepath.Base(s.path), SourceBytes: s.stats.SourcePMTilesBytes,
		TileCount: s.stats.Tiles, TileBytes: s.stats.TileBytes,
		MaxTileBytes: s.stats.MaxTileBytes, Metadata: metadata,
	}, nil
}

func (s *pmtilesTileSource) ScanTiles(ctx context.Context, visit func(tiles.Tile) error) error {
	if s == nil || s.archive == nil {
		return errors.New("PMTiles source is closed")
	}
	return s.archive.EachTile(ctx, func(tile pmtiles.Tile) error {
		return visit(tiles.Tile{
			Key:  tiles.Key{Z: int(tile.Z), X: int(tile.X), Y: int(tile.TMSRow())},
			Data: tile.Data,
		})
	})
}

// pmtilesStagingSource is a private, temporary flat MBTiles source produced
// from a PMTiles archive. It must be removed after ImportMBTiles consumed it.
type pmtilesStagingSource struct {
	Path    string
	Stats   pmtilesStagingStats
	cleanup func()
}

func (s pmtilesStagingSource) Close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

const pmtilesCommitBatch = 4_096

// stagePMTiles converts a PMTiles v3 archive into a temporary flat MBTiles
// source so the ordinary, fully validated MBTiles import path can publish it.
// PMTiles is an exchange format here, exactly like MBTiles: tinyTiles reads it
// and owns neither its production nor its map semantics.
//
// Tile payloads are copied byte for byte. The one transformation applied is
// the required coordinate-convention change: PMTiles addresses tiles in XYZ
// (origin top left) while MBTiles rows are TMS (origin bottom left), so every
// row is flipped exactly once here and never again downstream.
func stagePMTiles(ctx context.Context, source, artifact string) (pmtilesStagingSource, error) {
	if err := ctx.Err(); err != nil {
		return pmtilesStagingSource{}, err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("stat PMTiles source: %w", err)
	}
	archive, err := pmtiles.Open(source)
	if err != nil {
		return pmtilesStagingSource{}, err
	}
	defer archive.Close()
	header := archive.Header()
	if encoding := header.TileCompression.ContentEncoding(); encoding != "" && encoding != "gzip" {
		return pmtilesStagingSource{}, fmt.Errorf(
			"PMTiles archive stores tiles with %s compression; tinyTiles imports uncompressed or gzip tile payloads",
			header.TileCompression)
	}
	metadata, err := pmtilesMetadataRows(archive, header)
	if err != nil {
		return pmtilesStagingSource{}, err
	}

	parent := filepath.Dir(filepath.Clean(artifact))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("create PMTiles staging parent: %w", err)
	}
	work, err := os.MkdirTemp(parent, ".tinytiles-pmtiles-*")
	if err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("create PMTiles staging workspace: %w", err)
	}
	// The staging file keeps the source stem with an .mbtiles suffix: tinySQL
	// records the basename in manifest.Source, and the true PMTiles origin is
	// recorded separately in provenance.
	stem := strings.TrimSuffix(filepath.Base(filepath.Clean(source)), filepath.Ext(source))
	if stem == "" {
		stem = "pmtiles-source"
	}
	result := pmtilesStagingSource{
		Path: filepath.Join(work, stem+".mbtiles"),
		Stats: pmtilesStagingStats{
			SourcePMTilesBytes: sourceInfo.Size(),
			MinZoom:            int(header.MinZoom),
			MaxZoom:            int(header.MaxZoom),
			Format:             metadata["format"],
			ContentEncoding:    header.TileCompression.ContentEncoding(),
		},
		cleanup: func() { _ = os.RemoveAll(work) },
	}
	keep := false
	defer func() {
		if !keep {
			result.Close()
		}
	}()

	targetDB, err := sql.Open("sqlite", result.Path)
	if err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("create PMTiles staging source: %w", err)
	}
	targetOpen := true
	defer func() {
		if targetOpen {
			_ = targetDB.Close()
		}
	}()
	targetDB.SetMaxOpenConns(1)
	targetDB.SetMaxIdleConns(1)
	if err := createPMTilesStagingSchema(ctx, targetDB); err != nil {
		return pmtilesStagingSource{}, err
	}
	if err := writePMTilesMetadata(ctx, targetDB, metadata); err != nil {
		return pmtilesStagingSource{}, err
	}
	stats, err := copyPMTilesTiles(ctx, archive, targetDB)
	if err != nil {
		return pmtilesStagingSource{}, err
	}
	result.Stats.Tiles = stats.Tiles
	result.Stats.TileBytes = stats.TileBytes
	if result.Stats.Tiles == 0 {
		return pmtilesStagingSource{}, fmt.Errorf("PMTiles archive %s contains no tiles", filepath.Base(source))
	}
	if _, err := targetDB.ExecContext(ctx, `CREATE UNIQUE INDEX tile_index ON tiles(zoom_level, tile_column, tile_row)`); err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("index PMTiles staging source: %w", err)
	}
	if err := targetDB.Close(); err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("close PMTiles staging source: %w", err)
	}
	targetOpen = false
	stagingInfo, err := os.Stat(result.Path)
	if err != nil {
		return pmtilesStagingSource{}, fmt.Errorf("stat PMTiles staging source: %w", err)
	}
	result.Stats.StagingMBTilesBytes = stagingInfo.Size()
	keep = true
	return result, nil
}

func createPMTilesStagingSchema(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{
		`PRAGMA journal_mode=OFF`,
		`PRAGMA synchronous=OFF`,
		`PRAGMA temp_store=MEMORY`,
		`PRAGMA cache_size=-16384`,
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize PMTiles staging source: %w", err)
		}
	}
	return nil
}

// pmtilesMetadataRows maps the archive header and its JSON metadata onto the
// MBTiles metadata rows tinyTiles already understands. Only documented keys
// are relayed; the archive's own producer owns their content.
func pmtilesMetadataRows(archive *pmtiles.Archive, header pmtiles.Header) (map[string]string, error) {
	rows := map[string]string{
		"minzoom": strconv.Itoa(int(header.MinZoom)),
		"maxzoom": strconv.Itoa(int(header.MaxZoom)),
		"bounds": fmt.Sprintf("%g,%g,%g,%g",
			header.MinLongitude, header.MinLatitude, header.MaxLongitude, header.MaxLatitude),
	}
	if header.CenterZoom > 0 || header.CenterLongitude != 0 || header.CenterLatitude != 0 {
		rows["center"] = fmt.Sprintf("%g,%g,%d", header.CenterLongitude, header.CenterLatitude, header.CenterZoom)
	}
	if format := header.TileType.MBTilesFormat(); format != "" {
		rows["format"] = format
	}
	// tinyTiles' server reads this key to set the tile Content-Encoding.
	if header.TileCompression == pmtiles.CompressionGzip {
		rows["kb:content_encoding"] = "gzip"
	}

	raw, err := archive.Metadata()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return rows, nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode PMTiles metadata JSON: %w", err)
	}
	for _, key := range []string{"name", "description", "attribution", "version", "type", "encoding", "format"} {
		value, found := decoded[key]
		if !found {
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			// A non-string value is relayed verbatim rather than dropped; MBTiles
			// metadata is a text table and the producer's intent is preserved.
			text = strings.TrimSpace(string(value))
		}
		if text == "" {
			continue
		}
		// The header's tile type is authoritative for format when it declared
		// one, because it also determined the payload bytes.
		if key == "format" && rows["format"] != "" {
			continue
		}
		rows[key] = text
	}
	// vector_layers and tilestats belong together in the standard MBTiles
	// "json" row, which the tinyTiles server relays into TileJSON.
	tilesetJSON := map[string]json.RawMessage{}
	for _, key := range []string{"vector_layers", "tilestats"} {
		if value, found := decoded[key]; found {
			tilesetJSON[key] = value
		}
	}
	if len(tilesetJSON) > 0 {
		encoded, err := json.Marshal(tilesetJSON)
		if err != nil {
			return nil, fmt.Errorf("encode PMTiles vector layer metadata: %w", err)
		}
		rows["json"] = string(encoded)
	}
	return rows, nil
}

func writePMTilesMetadata(ctx context.Context, db *sql.DB, rows map[string]string) error {
	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin PMTiles metadata transaction: %w", err)
	}
	statement, err := transaction.PrepareContext(ctx, `INSERT INTO metadata (name, value) VALUES (?, ?)`)
	if err != nil {
		_ = transaction.Rollback()
		return fmt.Errorf("prepare PMTiles metadata insert: %w", err)
	}
	defer statement.Close()
	for name, value := range rows {
		if _, err := statement.ExecContext(ctx, name, value); err != nil {
			_ = transaction.Rollback()
			return fmt.Errorf("write PMTiles metadata %q: %w", name, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit PMTiles metadata: %w", err)
	}
	return nil
}

func copyPMTilesTiles(ctx context.Context, archive *pmtiles.Archive, db *sql.DB) (pmtilesStagingStats, error) {
	var stats pmtilesStagingStats
	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin PMTiles tile transaction: %w", err)
	}
	statement, err := transaction.PrepareContext(ctx, `INSERT INTO tiles (zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = transaction.Rollback()
		return stats, fmt.Errorf("prepare PMTiles tile insert: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if statement != nil {
				_ = statement.Close()
			}
			_ = transaction.Rollback()
		}
	}()

	pending := 0
	err = archive.EachTile(ctx, func(tile pmtiles.Tile) error {
		// PMTiles rows are XYZ; MBTiles rows are TMS. This is the single row
		// flip in the whole conversion.
		if _, err := statement.ExecContext(ctx, int(tile.Z), int(tile.X), int(tile.TMSRow()), tile.Data); err != nil {
			return fmt.Errorf("write tile %d/%d/%d: %w", tile.Z, tile.X, tile.Y, err)
		}
		stats.Tiles++
		stats.TileBytes += int64(len(tile.Data))
		pending++
		if pending < pmtilesCommitBatch {
			return nil
		}
		if err := statement.Close(); err != nil {
			return fmt.Errorf("close PMTiles tile statement: %w", err)
		}
		statement = nil
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit PMTiles tiles: %w", err)
		}
		next, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin PMTiles tile transaction: %w", err)
		}
		transaction = next
		statement, err = transaction.PrepareContext(ctx, `INSERT INTO tiles (zoom_level, tile_column, tile_row, tile_data) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare PMTiles tile insert: %w", err)
		}
		pending = 0
		return nil
	})
	if err != nil {
		return pmtilesStagingStats{}, err
	}
	if statement != nil {
		if err := statement.Close(); err != nil {
			return pmtilesStagingStats{}, fmt.Errorf("close PMTiles tile statement: %w", err)
		}
		statement = nil
	}
	if err := transaction.Commit(); err != nil {
		return pmtilesStagingStats{}, fmt.Errorf("commit PMTiles tiles: %w", err)
	}
	committed = true
	return stats, nil
}

func pmtilesImportProvenance(provenance map[string]any, source string, stats pmtilesStagingStats) map[string]any {
	return pmtilesProvenance(provenance, source, stats, "pmtiles-v3-to-mbtiles", true)
}

func pmtilesDirectImportProvenance(provenance map[string]any, source string, stats pmtilesStagingStats) map[string]any {
	return pmtilesProvenance(provenance, source, stats, "pmtiles-v3-direct-stream", false)
}

func pmtilesProvenance(provenance map[string]any, source string, stats pmtilesStagingStats, mode string, staged bool) map[string]any {
	out := make(map[string]any, len(provenance)+1)
	for key, value := range provenance {
		out[key] = value
	}
	entry := map[string]any{
		"mode":                 mode,
		"source":               filepath.Base(filepath.Clean(source)),
		"source_pmtiles_bytes": stats.SourcePMTilesBytes,
		"tiles":                stats.Tiles,
		"tile_bytes":           stats.TileBytes,
		"minzoom":              stats.MinZoom,
		"maxzoom":              stats.MaxZoom,
		"coordinates":          "xyz-source-flipped-to-tms",
		"payloads":             "byte-identical",
	}
	if staged {
		entry["staging_mbtiles_bytes"] = stats.StagingMBTilesBytes
	}
	if stats.Format != "" {
		entry["format"] = stats.Format
	}
	if stats.ContentEncoding != "" {
		entry["tile_content_encoding"] = stats.ContentEncoding
	}
	out["tinytiles_pmtiles_import"] = entry
	return out
}

func pmtilesDirectStatsLine(stats pmtilesStagingStats) string {
	line := fmt.Sprintf("pmtiles mode=direct tiles=%d payload=%dB zooms=%d-%d", stats.Tiles, stats.TileBytes, stats.MinZoom, stats.MaxZoom)
	if stats.Format != "" {
		line += " format=" + stats.Format
	}
	if stats.ContentEncoding != "" {
		line += " tile-encoding=" + stats.ContentEncoding
	}
	return line
}

func pmtilesStatsLine(stats pmtilesStagingStats) string {
	line := fmt.Sprintf(
		"pmtiles tiles=%d payload=%dB zooms=%d-%d staging=%dB",
		stats.Tiles, stats.TileBytes, stats.MinZoom, stats.MaxZoom, stats.StagingMBTilesBytes)
	if stats.Format != "" {
		line += " format=" + stats.Format
	}
	if stats.ContentEncoding != "" {
		line += " tile-encoding=" + stats.ContentEncoding
	}
	return line
}
