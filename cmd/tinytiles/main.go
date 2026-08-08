//go:build sqliteimport && !js && !wasm && !baremetal

// tinytiles is the command-line entry point for the standalone tinyTiles
// project. It deliberately uses only tinySQL's public artifact API so this
// directory can move to its own repository without importing internal code.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tinytiles "github.com/Karte-Bayern/tinyTiles"
	"github.com/Karte-Bayern/tinyTiles/internal/pmtiles"
	_ "github.com/SimonWaldherr/tinySQL/importer"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

const (
	defaultImportBatchSize = 1_000
	// defaultAutoImportBatchSize is large enough to amortize paged-index
	// checkpoints without making one cancellation boundary excessively long.
	// The actual automatic value is further capped by a bounded source sample
	// and the caller's memory limit before the importer is started.
	defaultAutoImportBatchSize       = 8_192
	importBatchSampleRows            = 1_024
	importBatchSampleHeadroom        = 8
	importBatchMinimumTileSize int64 = 16 << 10

	// tinySQL's artifact importer reserves a fixed streaming buffer plus this
	// per-row bookkeeping when it performs its preflight memory gate. Keep the
	// automatic CLI batch conservatively within that bounded model; the
	// importer remains the authoritative final preflight check.
	importBatchFixedMemory    int64 = 2 << 20
	importBatchPerRowOverhead int64 = 768
)

func commandImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	batch := fs.Int("batch", defaultImportBatchSize, "rows per bounded import batch; 0 enables automatic tuning within the memory limit")
	memory := fs.Int64("max-memory", 64<<20, "maximum tinySQL cache budget in bytes")
	reserve := fs.Int64("min-free", 1<<30, "required free disk reserve in bytes")
	schema := fs.String("schema", "auto", "artifact schema: auto, flat, normalized")
	compact := fs.Bool("compact", false, "losslessly deduplicate equal tile payloads into a normalized artifact (uses temporary disk)")
	replace := fs.Bool("replace", false, "atomically replace an existing artifact")
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: tinytiles import [flags] source.mbtiles|source.pmtiles dataset.ttiles/")
		return 2
	}
	artifactSchema, err := parseSchema(*schema)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *compact && artifactSchema == tiles.SchemaFlat {
		fmt.Fprintln(stderr, "tinytiles import: --compact requires schema auto or normalized; schema flat would discard payload deduplication")
		return 2
	}
	if *batch < 0 || *memory <= 0 || *reserve < 0 {
		fmt.Fprintln(stderr, "tinytiles import: batch must be zero (automatic) or positive; max-memory must be positive; min-free must not be negative")
		return 2
	}
	if err := validateImportPaths(fs.Arg(0), fs.Arg(1)); err != nil {
		fmt.Fprintf(stderr, "tinytiles import: %v\n", err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	result, err := importArtifactWithProvenanceAndCompact(ctx, fs.Arg(0), fs.Arg(1), artifactSchema, *batch, *memory, *reserve, *replace, nil, *compact, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles import: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "artifact=%s schema=%s\n", result.ArtifactPath, result.Info.Schema)
	return 0
}

// validateImportPaths prevents --replace from turning the input MBTiles file
// into the artifact directory. samePath catches ordinary and symlink aliases;
// os.SameFile additionally catches hard links on filesystems that support them.
func validateImportPaths(source, artifact string) error {
	if err := requireRegularFile("import source", source); err != nil {
		return err
	}
	same, err := samePath(source, artifact)
	if err != nil {
		return fmt.Errorf("resolve source and artifact paths: %w", err)
	}
	if same {
		return fmt.Errorf("artifact path %q must not refer to import source %q", artifact, source)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat import source %q: %w", source, err)
	}
	artifactInfo, err := os.Stat(artifact)
	if err == nil && os.SameFile(sourceInfo, artifactInfo) {
		return fmt.Errorf("artifact path %q must not refer to import source %q", artifact, source)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat artifact path %q: %w", artifact, err)
	}
	if within, err := pathWithin(source, artifact); err != nil {
		return fmt.Errorf("resolve source and artifact paths: %w", err)
	} else if within {
		return fmt.Errorf("artifact path %q must not contain import source %q", artifact, source)
	}
	if within, err := pathWithin(artifact, source); err != nil {
		return fmt.Errorf("resolve source and artifact paths: %w", err)
	} else if within {
		return fmt.Errorf("artifact path %q must not be inside import source %q", artifact, source)
	}
	return nil
}

func importArtifact(ctx context.Context, source, artifact string, schema tiles.Schema, batch int, memory, reserve int64, replace bool, stdout io.Writer) (*tiles.ImportResult, error) {
	return importArtifactWithProvenanceAndCompact(ctx, source, artifact, schema, batch, memory, reserve, replace, nil, false, stdout)
}

func importArtifactWithProvenance(ctx context.Context, source, artifact string, schema tiles.Schema, batch int, memory, reserve int64, replace bool, provenance map[string]any, stdout io.Writer) (*tiles.ImportResult, error) {
	return importArtifactWithProvenanceAndCompact(ctx, source, artifact, schema, batch, memory, reserve, replace, provenance, false, stdout)
}

func importArtifactWithProvenanceAndCompact(ctx context.Context, source, artifact string, schema tiles.Schema, batch int, memory, reserve int64, replace bool, provenance map[string]any, compact bool, stdout io.Writer) (*tiles.ImportResult, error) {
	start := time.Now()
	if batch < 0 {
		return nil, errors.New("import batch must not be negative")
	}
	if compact && schema == tiles.SchemaFlat {
		return nil, errors.New("compact import requires schema auto or normalized; schema flat would discard payload deduplication")
	}
	// A default PMTiles v3 import streams directly into the artifact writer.
	// Explicit normalized/compact imports retain the older staging path because
	// they require global payload deduplication. Detection is by header magic,
	// not file extension.
	if pmtiles.IsArchive(source) {
		fmt.Fprintln(stdout, "phase=pmtiles")
		if !compact && schema != tiles.SchemaNormalized {
			direct, err := openPMTilesTileSource(ctx, source)
			if err != nil {
				return nil, err
			}
			defer direct.Close()
			fmt.Fprintln(stdout, pmtilesDirectStatsLine(direct.stats))
			provenance = pmtilesDirectImportProvenance(provenance, source, direct.stats)
			resolvedBatch, adjusted := tileStreamBatchSize(batch, direct.stats.MaxTileBytes, memory)
			if adjusted {
				fmt.Fprintf(stdout, "batch-adjustment requested=%d resolved=%d reason=bounded-tile-stream-memory\n", batch, resolvedBatch)
			}
			batch = resolvedBatch
			result, err := tiles.ImportTiles(ctx, direct, artifact, &tiles.ImportOptions{
				Schema: tiles.SchemaFlat, BatchSize: batch, MaxMemoryBytes: memory,
				MinFreeBytes: reserve, Provenance: provenance,
				ProgressEvery: time.Second, ReplaceExisting: replace,
				Progress: func(progress tiles.Progress) {
					if progress.Phase == "preflight" && progress.Estimate != nil {
						e := progress.Estimate
						fmt.Fprintf(stdout, "preflight tiles=%d source=%dB estimated-disk=%dB working-set=%dB available-disk=%dB batch=%d\n", e.TileCount, e.SourceBytes, e.EstimatedDisk, e.EstimatedMemory, e.AvailableDisk, e.BatchSize)
					} else if progress.Phase == "published" {
						fmt.Fprintf(stdout, "published tiles=%d batches-of=%d\n", progress.RowsWritten, progress.BatchSize)
					}
				},
			})
			if err == nil {
				fmt.Fprintf(stdout, "import elapsed=%s\n", time.Since(start).Round(time.Millisecond))
			}
			return result, err
		}
		fmt.Fprintln(stdout, "pmtiles-mode=mbtiles-staging reason=normalized-or-compact")
		staging, err := stagePMTiles(ctx, source, artifact)
		if err != nil {
			return nil, err
		}
		defer staging.Close()
		fmt.Fprintln(stdout, pmtilesStatsLine(staging.Stats))
		provenance = pmtilesImportProvenance(provenance, source, staging.Stats)
		source = staging.Path
	}
	if !compact {
		resolvedSchema, resolution, err := resolveAutoArtifactSchema(ctx, source, schema)
		if err != nil {
			return nil, err
		}
		schema = resolvedSchema
		if resolution != "" {
			fmt.Fprintln(stdout, resolution)
		}
	}
	if batch == 0 {
		var err error
		batch, err = autoImportBatchSize(ctx, source, memory)
		if err != nil {
			return nil, err
		}
	}
	importSource := source
	if compact {
		fmt.Fprintln(stdout, "phase=compact")
		staging, err := compactMBTiles(ctx, source, artifact)
		if err != nil {
			return nil, err
		}
		defer staging.Close()
		fmt.Fprintln(stdout, compactStatsLine(staging.Stats))
		importSource = staging.Path
		schema = tiles.SchemaNormalized
		provenance = compactImportProvenance(provenance, source, staging.Stats)
	}
	result, err := tiles.ImportMBTiles(ctx, importSource, artifact, &tiles.ImportOptions{
		Schema: schema, BatchSize: batch, MaxMemoryBytes: memory, MinFreeBytes: reserve, Provenance: provenance, ProgressEvery: time.Second, ReplaceExisting: replace,
		Progress: func(progress tiles.Progress) {
			if progress.Phase == "preflight" && progress.Estimate != nil {
				e := progress.Estimate
				fmt.Fprintf(stdout, "preflight tiles=%d source=%dB estimated-disk=%dB working-set=%dB available-disk=%dB batch=%d\n", e.TileCount, e.SourceBytes, e.EstimatedDisk, e.EstimatedMemory, e.AvailableDisk, e.BatchSize)
			} else if progress.Phase == "published" {
				fmt.Fprintf(stdout, "published tiles=%d batches-of=%d\n", progress.RowsWritten, progress.BatchSize)
			}
		},
	})
	if err == nil {
		fmt.Fprintf(stdout, "import elapsed=%s\n", time.Since(start).Round(time.Millisecond))
		if compact {
			if bytes, sizeErr := artifactDirectoryBytes(artifact); sizeErr == nil {
				fmt.Fprintf(stdout, "compact artifact-bytes=%dB\n", bytes)
			}
		}
	}
	return result, err
}

func tileStreamBatchSize(requested int, maxTileBytes, maxMemory int64) (resolved int, adjusted bool) {
	available := maxMemory - importBatchFixedMemory
	if available <= 0 || maxTileBytes < 0 || maxTileBytes > math.MaxInt64-importBatchPerRowOverhead {
		if requested == 0 {
			return 1, false
		}
		return 1, requested != 1
	}
	perTile := maxTileBytes + importBatchPerRowOverhead
	batch := available / perTile
	if batch < 1 {
		batch = 1
	}
	if batch > defaultAutoImportBatchSize {
		batch = defaultAutoImportBatchSize
	}
	if requested == 0 {
		return int(batch), false
	}
	if int64(requested) > batch {
		return int(batch), true
	}
	return requested, false
}

// resolveAutoArtifactSchema avoids the normalized map→image join on the
// serving path when a normalized MBTiles source has no tile_id reuse at all.
// It deliberately applies only to automatic, non-compact imports: an explicit
// representation remains the caller's choice, and --compact must retain its
// normalized deduplication table even before the compact staging source exists.
func resolveAutoArtifactSchema(ctx context.Context, source string, schema tiles.Schema) (tiles.Schema, string, error) {
	if schema != tiles.SchemaAuto {
		return schema, "", nil
	}
	if err := ctx.Err(); err != nil {
		return schema, "", err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(source)+"?mode=ro&immutable=1&cache=private")
	if err != nil {
		return schema, "", fmt.Errorf("open MBTiles source for schema resolution: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return schema, "", fmt.Errorf("ping MBTiles source for schema resolution: %w", err)
	}
	flat, err := sqliteTableExistsContext(ctx, db, "tiles")
	if err != nil {
		return schema, "", err
	}
	if flat {
		return schema, "", nil
	}
	mapTable, err := sqliteTableExistsContext(ctx, db, "map")
	if err != nil {
		return schema, "", err
	}
	images, err := sqliteTableExistsContext(ctx, db, "images")
	if err != nil {
		return schema, "", err
	}
	if !mapTable || !images {
		// Defer malformed or unsupported source diagnostics to the importer,
		// which has the complete source-schema error handling.
		return schema, "", nil
	}
	var mapRows, uniqueTileIDs int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT tile_id) FROM map`).Scan(&mapRows, &uniqueTileIDs); err != nil {
		return schema, "", fmt.Errorf("measure normalized MBTiles tile reuse for schema resolution: %w", err)
	}
	if mapRows == uniqueTileIDs {
		return tiles.SchemaFlat, fmt.Sprintf("schema-resolution requested=auto resolved=flat map=%d unique-tile-ids=%d", mapRows, uniqueTileIDs), nil
	}
	return schema, fmt.Sprintf("schema-resolution requested=auto resolved=auto map=%d unique-tile-ids=%d", mapRows, uniqueTileIDs), nil
}

// autoImportBatchSize selects a fast, bounded import batch without adding a
// second whole-MBTiles scan before tinySQL's own preflight. It samples at most
// importBatchSampleRows tile lengths and uses conservative headroom. The
// source is opened read-only without creating an artifact; the importer
// remains the authoritative memory gate for all source rows.
func autoImportBatchSize(ctx context.Context, source string, maxMemory int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maxMemory <= 0 {
		return 0, errors.New("import max-memory must be positive")
	}
	sampledTile, err := sampledMBTileBytes(ctx, source)
	if err != nil {
		return 0, err
	}
	if sampledTile > (1<<63-1)/importBatchSampleHeadroom {
		return defaultImportBatchSize, nil
	}
	perTile := sampledTile * importBatchSampleHeadroom
	if perTile < importBatchMinimumTileSize {
		perTile = importBatchMinimumTileSize
	}
	if perTile > (1<<63-1)-importBatchPerRowOverhead {
		return defaultImportBatchSize, nil
	}
	perTile += importBatchPerRowOverhead
	available := maxMemory - importBatchFixedMemory
	if available <= 0 || perTile <= 0 {
		return defaultImportBatchSize, nil
	}
	batch := available / perTile
	if batch < 1 {
		return defaultImportBatchSize, nil
	}
	if batch > defaultAutoImportBatchSize {
		return defaultAutoImportBatchSize, nil
	}
	return int(batch), nil
}

func sampledMBTileBytes(ctx context.Context, source string) (int64, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(source)+"?mode=ro&immutable=1")
	if err != nil {
		return 0, fmt.Errorf("open MBTiles source for automatic batch sizing: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return 0, fmt.Errorf("ping MBTiles source for automatic batch sizing: %w", err)
	}

	flat, err := sqliteTableExistsContext(ctx, db, "tiles")
	if err != nil {
		return 0, err
	}
	if flat {
		return sampledMaxTileBytes(ctx, db, "tiles")
	}
	images, err := sqliteTableExistsContext(ctx, db, "images")
	if err != nil {
		return 0, err
	}
	if images {
		return sampledMaxTileBytes(ctx, db, "images")
	}
	return 0, errors.New("MBTiles source has neither tiles nor images table for automatic batch sizing")
}

func sqliteTableExistsContext(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect MBTiles schema for automatic batch sizing: %w", err)
	}
	return true, nil
}

func sampledMaxTileBytes(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var max sql.NullInt64
	var query string
	switch table {
	case "tiles":
		query = "SELECT MAX(length(tile_data)) FROM (SELECT tile_data FROM tiles LIMIT ?)"
	case "images":
		query = "SELECT MAX(length(tile_data)) FROM (SELECT tile_data FROM images LIMIT ?)"
	default:
		return 0, fmt.Errorf("unsupported MBTiles tile table %q", table)
	}
	if err := db.QueryRowContext(ctx, query, importBatchSampleRows).Scan(&max); err != nil {
		return 0, fmt.Errorf("measure largest MBTiles tile for automatic batch sizing: %w", err)
	}
	if !max.Valid || max.Int64 < 0 {
		return 0, nil
	}
	return max.Int64, nil
}

func commandBenchmark(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "SQLite MBTiles reference")
	artifact := fs.String("artifact", "", "published tinyTiles artifact")
	requests := fs.Int("requests", 512, "number of deterministic benchmark lookups")
	memory := fs.Int64("max-memory", tinytiles.DefaultReaderMemoryBytes, "per-reader cache budget in bytes")
	readers := fs.Int("readers", 8, "independent readers used for parallel measurements")
	cold := fs.Bool("cold", false, "measure a fresh-reader corpus before parity and warm-up")
	coldRuns := fs.Int("cold-runs", 5, "complete fresh-reader corpus runs per profile; percentile results use their median")
	coldMaxP95Ratio := fs.Float64("cold-max-p95-ratio", 0, "fail fresh-reader corpus measurements above this tinyTiles/SQLite p95 ratio (0 disables; nonzero enables --cold)")
	coldRequest := fs.Bool("cold-request", false, "measure one fresh SQLite connection or Dataset per requested tile before parity and warm-up")
	coldRequestMaxP95Ratio := fs.Float64("cold-request-max-p95-ratio", 0, "fail application-cold request measurements above this tinyTiles/SQLite lookup p95 ratio (0 disables; nonzero enables --cold-request)")
	seed := fs.Int64("seed", 0x71A5, "deterministic corpus shuffle seed")
	if fs.Parse(args) != nil || *source == "" || *artifact == "" || *requests < 10 || *readers < 1 || *memory <= 0 || *coldRuns < 1 || *coldMaxP95Ratio < 0 || *coldRequestMaxP95Ratio < 0 || math.IsNaN(*coldMaxP95Ratio) || math.IsInf(*coldMaxP95Ratio, 0) || math.IsNaN(*coldRequestMaxP95Ratio) || math.IsInf(*coldRequestMaxP95Ratio, 0) {
		fmt.Fprintln(stderr, "usage: tinytiles benchmark --source source.mbtiles --artifact dataset.ttiles/ [--requests 512]")
		return 2
	}
	coldEnabled := *cold || *coldMaxP95Ratio > 0
	coldRequestEnabled := *coldRequest || *coldRequestMaxP95Ratio > 0
	sqliteOpenStart := time.Now()
	db, err := openBenchmarkSQLite(*source, *readers)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: open SQLite: %v\n", err)
		return 1
	}
	sqliteOpen := time.Since(sqliteOpenStart)
	benchmarkSource, err := detectBenchmarkSource(db)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: %v\n", err)
		_ = db.Close()
		return 1
	}
	corpus, err := benchmarkCorpus(db, benchmarkSource, *requests, *seed)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: %v\n", err)
		_ = db.Close()
		return 1
	}
	if err := randomizeUniqueBenchmarkCorpus(corpus, *seed); err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: %v\n", err)
		_ = db.Close()
		return 1
	}

	// A fresh-reader corpus profile must not inherit the corpus selection connection, a
	// prepared statement, artifact reader pages, parity reads or a warm-up.
	// Closing this connection resets modernc SQLite's per-connection page cache;
	// the OS filesystem cache deliberately remains outside this benchmark's
	// control and is reported as such below.
	var coldMeasurements []freshReaderCorpusMeasurement
	var coldFailures []coldP95Failure
	var coldRequestResult *coldRequestMeasurement
	var coldRequestFailure *coldRequestP95Failure
	if coldEnabled || coldRequestEnabled {
		if err := db.Close(); err != nil {
			fmt.Fprintf(stderr, "tinytiles benchmark: close corpus SQLite reader before cold measurements: %v\n", err)
			return 1
		}
		db = nil
		if coldRequestEnabled {
			measurement, err := measureColdRequests(*source, *artifact, benchmarkSource, corpus, *memory, *coldRuns)
			if err != nil {
				fmt.Fprintf(stderr, "tinytiles benchmark: application-cold request: %v\n", err)
				return 1
			}
			coldRequestResult = &measurement
			if *coldRequestMaxP95Ratio > 0 && float64(measurement.tiny.lookup.p95) > float64(measurement.sqlite.lookup.p95)*(*coldRequestMaxP95Ratio) {
				coldRequestFailure = &coldRequestP95Failure{sqlite: measurement.sqlite.lookup.p95, tiny: measurement.tiny.lookup.p95}
			}
		}
		if coldEnabled {
			coldMeasurements, err = measureFreshReaderCorpus(*source, *artifact, benchmarkSource, corpus, *memory, benchmarkReaderCounts(*readers), *coldRuns)
			if err != nil {
				fmt.Fprintf(stderr, "tinytiles benchmark: fresh-reader corpus: %v\n", err)
				return 1
			}
			if *coldMaxP95Ratio > 0 {
				coldFailures = coldP95Failures(coldMeasurements, *coldMaxP95Ratio)
			}
		}
		// Reopen a normal reference connection for the established parity and
		// warmed profile. This preserves the cold phase's isolation.
		db, err = openBenchmarkSQLite(*source, *readers)
		if err != nil {
			fmt.Fprintf(stderr, "tinytiles benchmark: reopen SQLite after fresh-reader corpus: %v\n", err)
			return 1
		}
	}
	defer db.Close()
	tinyOpenStart := time.Now()
	dataset, err := tinytiles.Open(context.Background(), *artifact, tinytiles.OpenOptions{Readers: *readers, MaxMemoryBytes: *memory})
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: open artifact: %v\n", err)
		return 1
	}
	defer dataset.Close()
	tinyOpen := time.Since(tinyOpenStart)
	stmt, err := db.Prepare(benchmarkSource.lookupSQL)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: prepare SQLite: %v\n", err)
		return 1
	}
	defer stmt.Close()
	sqliteLookup := func(tile benchmarkTile) error {
		var data []byte
		if err := stmt.QueryRow(tile.z, tile.x, tile.y).Scan(&data); err != nil {
			return err
		}
		if len(data) != tile.size {
			return errors.New("SQLite tile length differs from corpus")
		}
		return nil
	}
	tinyLookup := func(tile benchmarkTile) error {
		value, found, err := dataset.LookupTMS(context.Background(), tiles.Key{Z: tile.z, X: tile.x, Y: tile.y})
		if err != nil {
			return err
		}
		if !found || len(value.Data) != tile.size {
			return errors.New("tinyTiles tile length differs from corpus")
		}
		return nil
	}
	if err := verifyBenchmarkParity(corpus, stmt, dataset); err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: parity: %v\n", err)
		return 1
	}
	artifactInfo := dataset.Info()
	fullTiles, fullMetadata, err := verifyFullBenchmarkParity(db, benchmarkSource, artifactInfo)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: full parity: %v\n", err)
		return 1
	}
	if err := warm(corpus, sqliteLookup); err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: warm SQLite: %v\n", err)
		return 1
	}
	if err := warm(corpus, tinyLookup); err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: warm artifact: %v\n", err)
		return 1
	}
	sqliteStats, err := measure(corpus, sqliteLookup)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: SQLite: %v\n", err)
		return 1
	}
	tinyStats, err := measure(corpus, tinyLookup)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: artifact: %v\n", err)
		return 1
	}
	sourceBytes, artifactBytes, sizeErr := benchmarkSizes(*source, *artifact)
	if sizeErr != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: sizes: %v\n", sizeErr)
		return 1
	}
	fmt.Fprintln(stdout, "resource\tSQLite\ttinyTiles")
	fmt.Fprintf(stdout, "open\t%s\t%s\n", sqliteOpen, tinyOpen)
	fmt.Fprintf(stdout, "schema\t%s\t%s\n", benchmarkSource.schema, artifactInfo.Schema)
	fmt.Fprintf(stdout, "bytes\t%d\t%d\n", sourceBytes, artifactBytes)
	fmt.Fprintf(stdout, "size-ratio\t1.000\t%.3f\n", float64(artifactBytes)/float64(sourceBytes))
	fmt.Fprintf(stdout, "sample-parity\t%d/%d\tPASS\n", len(corpus), len(corpus))
	fmt.Fprintf(stdout, "full-parity\ttiles=%d metadata=%d\tPASS\n", fullTiles, fullMetadata)
	if coldEnabled {
		fmt.Fprintf(stdout, "cold-mode\tfresh Dataset readers and SQLite statements per profile; randomized unique corpus; OS filesystem cache not forcibly evicted\tseed=%d unique=%d runs=%d percentile-aggregation=median\n", *seed, len(corpus), *coldRuns)
		fmt.Fprintf(stdout, "cold-aggregation\tmedian of per-run p50/p95/p99 across %d complete fresh-reader runs; SQLite/tinyTiles measurement order alternates; gate uses median p95\n", *coldRuns)
	}
	if coldRequestResult != nil {
		fmt.Fprintf(stdout, "cold-request-mode\tone fresh SQLite connection or Dataset per requested tile; artifact validated once before timing; randomized unique corpus; OS filesystem cache not forcibly evicted\tseed=%d unique=%d runs=%d percentile-aggregation=median\n", *seed, len(corpus), *coldRuns)
		fmt.Fprintf(stdout, "cold-request-aggregation\tmedian of per-run p50/p95/p99 across %d complete application-cold runs; backend order alternates for every request; gate uses lookup p95\n", *coldRuns)
		fmt.Fprintln(stdout, "cold-request\tbackend\tready-p50\tready-p95\tlookup-p50\tlookup-p95\tlookup-p99\ttotal-p50\ttotal-p95\ttotal-p99")
		printColdRequestLatency(stdout, "SQLite", coldRequestResult.sqlite)
		printColdRequestLatency(stdout, "tinyTiles", coldRequestResult.tiny)
	}
	fmt.Fprintln(stdout, "workload\tbackend\treaders\tp50\tp95\tp99")
	for _, measurement := range coldMeasurements {
		printLatency(stdout, "fresh-reader-corpus", "SQLite", measurement.readers, measurement.sqlite)
		printLatency(stdout, "fresh-reader-corpus", "tinyTiles", measurement.readers, measurement.tiny)
	}
	printLatency(stdout, "point", "SQLite", 1, sqliteStats)
	printLatency(stdout, "point", "tinyTiles", 1, tinyStats)
	for _, parallelism := range []int{4, 8} {
		if parallelism > *readers {
			continue
		}
		sqliteParallel, err := measureParallel(corpus, parallelism, sqliteLookup)
		if err != nil {
			fmt.Fprintf(stderr, "tinytiles benchmark: SQLite parallel=%d: %v\n", parallelism, err)
			return 1
		}
		tinyParallel, err := measureParallel(corpus, parallelism, tinyLookup)
		if err != nil {
			fmt.Fprintf(stderr, "tinytiles benchmark: tinyTiles parallel=%d: %v\n", parallelism, err)
			return 1
		}
		printLatency(stdout, "point", "SQLite", parallelism, sqliteParallel)
		printLatency(stdout, "point", "tinyTiles", parallelism, tinyParallel)
	}
	rangeStmt, err := db.Prepare(benchmarkSource.rangeSQL)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: prepare SQLite range: %v\n", err)
		return 1
	}
	defer rangeStmt.Close()
	sqliteRange := sqliteRangeLookup(rangeStmt)
	tinyRange := tinyRangeLookup(dataset)
	if err := warm(corpus, sqliteRange); err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: warm SQLite range: %v\n", err)
		return 1
	}
	if err := warm(corpus, tinyRange); err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: warm artifact range: %v\n", err)
		return 1
	}
	sqliteRangeStats, err := measure(corpus, sqliteRange)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: SQLite range: %v\n", err)
		return 1
	}
	tinyRangeStats, err := measure(corpus, tinyRange)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: tinyTiles range: %v\n", err)
		return 1
	}
	printLatency(stdout, "spatial-2x2", "SQLite", 1, sqliteRangeStats)
	printLatency(stdout, "spatial-2x2", "tinyTiles", 1, tinyRangeStats)
	if len(coldFailures) > 0 {
		for _, failure := range coldFailures {
			fmt.Fprintf(stderr, "tinytiles benchmark: FAIL cold median-p95 readers=%d %s exceeds %.3fx SQLite %s\n", failure.readers, failure.tiny, *coldMaxP95Ratio, failure.sqlite)
		}
		return 1
	}
	if coldRequestFailure != nil {
		fmt.Fprintf(stderr, "tinytiles benchmark: FAIL application-cold lookup median-p95 %s exceeds %.3fx SQLite %s\n", coldRequestFailure.tiny, *coldRequestMaxP95Ratio, coldRequestFailure.sqlite)
		return 1
	}
	if coldEnabled && *coldMaxP95Ratio > 0 {
		fmt.Fprintf(stdout, "cold-gate\tmedian-p95 <= %.3fx SQLite\tPASS\n", *coldMaxP95Ratio)
	}
	if coldRequestResult != nil && *coldRequestMaxP95Ratio > 0 {
		fmt.Fprintf(stdout, "cold-request-gate\tmedian-lookup-p95 <= %.3fx SQLite\tPASS\n", *coldRequestMaxP95Ratio)
	}
	if tinyStats.p95 > 2*sqliteStats.p95 {
		fmt.Fprintf(stderr, "tinytiles benchmark: FAIL p95 %s exceeds 2x SQLite %s\n", tinyStats.p95, sqliteStats.p95)
		return 1
	}
	fmt.Fprintln(stdout, "gate\tp95 <= 2x SQLite\tPASS")
	return 0
}

func parseSchema(value string) (tiles.Schema, error) {
	switch strings.ToLower(value) {
	case "auto":
		return tiles.SchemaAuto, nil
	case "flat":
		return tiles.SchemaFlat, nil
	case "normalized":
		return tiles.SchemaNormalized, nil
	default:
		return "", fmt.Errorf("unsupported schema %q (want auto, flat or normalized)", value)
	}
}

type benchmarkTile struct {
	z, x, y int
	size    int
}
type latencyStats struct{ p50, p95, p99 time.Duration }

// freshReaderCorpusMeasurement contains the per-percentile median across full
// runs with freshly opened artifact readers and SQLite connections/statements.
// It intentionally excludes process startup and OS page-cache eviction,
// neither of which is a tile-reader operation an application performs for each
// deployment.
type freshReaderCorpusMeasurement struct {
	readers int
	sqlite  latencyStats
	tiny    latencyStats
}

// coldRequestMeasurement records a request path in which no client-side
// reader or SQLite page cache survives from one coordinate to the next.
// Artifact validation happens once before timing, matching the publication
// lifecycle without turning a reader cache into a benchmark cache hit.
type coldRequestMeasurement struct {
	sqlite coldRequestStats
	tiny   coldRequestStats
}

type coldRequestStats struct {
	ready  latencyStats
	lookup latencyStats
	total  latencyStats
}

type coldP95Failure struct {
	readers int
	sqlite  time.Duration
	tiny    time.Duration
}

type coldRequestP95Failure struct {
	sqlite time.Duration
	tiny   time.Duration
}

type benchmarkSQLiteReader struct {
	db   *sql.DB
	stmt *sql.Stmt
}

func openBenchmarkSQLite(source string, readers int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(source)+"?mode=ro&immutable=1&cache=private")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(readers)
	db.SetMaxIdleConns(readers)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openBenchmarkSQLiteReaders creates one fully initialized SQLite reader and
// statement per benchmark worker. Using separate pools avoids accidentally
// sharing modernc SQLite's per-connection page cache between cold workers.
func openBenchmarkSQLiteReaders(source, lookupSQL string, readers int) ([]benchmarkSQLiteReader, error) {
	result := make([]benchmarkSQLiteReader, 0, readers)
	closeResult := func() {
		for _, reader := range result {
			if reader.stmt != nil {
				_ = reader.stmt.Close()
			}
			if reader.db != nil {
				_ = reader.db.Close()
			}
		}
	}
	for index := 0; index < readers; index++ {
		db, err := openBenchmarkSQLite(source, 1)
		if err != nil {
			closeResult()
			return nil, fmt.Errorf("open SQLite reader %d: %w", index+1, err)
		}
		stmt, err := db.Prepare(lookupSQL)
		if err != nil {
			_ = db.Close()
			closeResult()
			return nil, fmt.Errorf("prepare SQLite reader %d: %w", index+1, err)
		}
		result = append(result, benchmarkSQLiteReader{db: db, stmt: stmt})
	}
	return result, nil
}

func closeBenchmarkSQLiteReaders(readers []benchmarkSQLiteReader) {
	for _, reader := range readers {
		if reader.stmt != nil {
			_ = reader.stmt.Close()
		}
		if reader.db != nil {
			_ = reader.db.Close()
		}
	}
}

func printColdRequestLatency(writer io.Writer, backend string, stats coldRequestStats) {
	fmt.Fprintf(writer, "application-cold-request\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", backend, stats.ready.p50, stats.ready.p95, stats.lookup.p50, stats.lookup.p95, stats.lookup.p99, stats.total.p50, stats.total.p95, stats.total.p99)
}

// measureColdRequests is the strictest in-process reader comparison offered by
// the CLI. Each coordinate gets a new SQLite connection or one-reader Dataset,
// so neither backend reuses a client/page cache. This remains
// application-cold: clearing the host-wide filesystem cache would require a
// platform-specific privileged operation and is intentionally never hidden in
// a benchmark command.
func measureColdRequests(sourcePath, artifactPath string, source sqliteBenchmarkSource, corpus []benchmarkTile, memory int64, runs int) (coldRequestMeasurement, error) {
	if runs < 1 {
		return coldRequestMeasurement{}, errors.New("application-cold runs must be positive")
	}
	if len(corpus) == 0 {
		return coldRequestMeasurement{}, errors.New("application-cold corpus must not be empty")
	}
	// Validate outside the measured request path. A published artifact is
	// expected to be verified before it is made live; repeating that full audit
	// per request would benchmark an invalid deployment lifecycle instead.
	if _, err := tiles.ValidateArtifact(context.Background(), artifactPath); err != nil {
		return coldRequestMeasurement{}, fmt.Errorf("validate artifact before application-cold requests: %w", err)
	}
	sqliteRuns := make([]coldRequestStats, 0, runs)
	tinyRuns := make([]coldRequestStats, 0, runs)
	for run := 0; run < runs; run++ {
		sqlite, tiny, err := measureColdRequestRun(sourcePath, artifactPath, source, corpus, memory, run)
		if err != nil {
			return coldRequestMeasurement{}, fmt.Errorf("run %d/%d: %w", run+1, runs, err)
		}
		sqliteRuns = append(sqliteRuns, sqlite)
		tinyRuns = append(tinyRuns, tiny)
	}
	sqlite, err := medianColdRequestStats(sqliteRuns)
	if err != nil {
		return coldRequestMeasurement{}, fmt.Errorf("aggregate SQLite: %w", err)
	}
	tiny, err := medianColdRequestStats(tinyRuns)
	if err != nil {
		return coldRequestMeasurement{}, fmt.Errorf("aggregate tinyTiles: %w", err)
	}
	return coldRequestMeasurement{sqlite: sqlite, tiny: tiny}, nil
}

// measureColdRequestRun alternates the backend per coordinate and shifts that
// alternation for every run. That prevents a fixed scheduler, file-system, or
// CPU-state advantage for the backend always measured first.
func measureColdRequestRun(sourcePath, artifactPath string, source sqliteBenchmarkSource, corpus []benchmarkTile, memory int64, run int) (coldRequestStats, coldRequestStats, error) {
	sqliteReady := make([]time.Duration, 0, len(corpus))
	sqliteLookup := make([]time.Duration, 0, len(corpus))
	sqliteTotal := make([]time.Duration, 0, len(corpus))
	tinyReady := make([]time.Duration, 0, len(corpus))
	tinyLookup := make([]time.Duration, 0, len(corpus))
	tinyTotal := make([]time.Duration, 0, len(corpus))
	for index, tile := range corpus {
		measureSQLite := func() error {
			ready, lookup, total, err := measureColdRequestSQLite(sourcePath, source.lookupSQL, tile)
			if err != nil {
				return err
			}
			sqliteReady = append(sqliteReady, ready)
			sqliteLookup = append(sqliteLookup, lookup)
			sqliteTotal = append(sqliteTotal, total)
			return nil
		}
		measureTiny := func() error {
			ready, lookup, total, err := measureColdRequestTinyTiles(artifactPath, memory, tile)
			if err != nil {
				return err
			}
			tinyReady = append(tinyReady, ready)
			tinyLookup = append(tinyLookup, lookup)
			tinyTotal = append(tinyTotal, total)
			return nil
		}
		if (run+index)%2 == 0 {
			if err := measureSQLite(); err != nil {
				return coldRequestStats{}, coldRequestStats{}, fmt.Errorf("SQLite %d/%d/%d: %w", tile.z, tile.x, tile.y, err)
			}
			if err := measureTiny(); err != nil {
				return coldRequestStats{}, coldRequestStats{}, fmt.Errorf("tinyTiles %d/%d/%d: %w", tile.z, tile.x, tile.y, err)
			}
			continue
		}
		if err := measureTiny(); err != nil {
			return coldRequestStats{}, coldRequestStats{}, fmt.Errorf("tinyTiles %d/%d/%d: %w", tile.z, tile.x, tile.y, err)
		}
		if err := measureSQLite(); err != nil {
			return coldRequestStats{}, coldRequestStats{}, fmt.Errorf("SQLite %d/%d/%d: %w", tile.z, tile.x, tile.y, err)
		}
	}
	sqlite, err := coldRequestStatsFromDurations(sqliteReady, sqliteLookup, sqliteTotal)
	if err != nil {
		return coldRequestStats{}, coldRequestStats{}, fmt.Errorf("SQLite percentiles: %w", err)
	}
	tiny, err := coldRequestStatsFromDurations(tinyReady, tinyLookup, tinyTotal)
	if err != nil {
		return coldRequestStats{}, coldRequestStats{}, fmt.Errorf("tinyTiles percentiles: %w", err)
	}
	return sqlite, tiny, nil
}

func measureColdRequestSQLite(sourcePath, lookupSQL string, tile benchmarkTile) (ready, lookup, total time.Duration, err error) {
	totalStart := time.Now()
	db, err := openBenchmarkSQLite(sourcePath, 1)
	if err != nil {
		return 0, 0, 0, err
	}
	ready = time.Since(totalStart)
	lookupStart := time.Now()
	var data []byte
	err = db.QueryRowContext(context.Background(), lookupSQL, tile.z, tile.x, tile.y).Scan(&data)
	lookup = time.Since(lookupStart)
	total = time.Since(totalStart)
	closeErr := db.Close()
	if err != nil {
		return 0, 0, 0, err
	}
	if closeErr != nil {
		return 0, 0, 0, closeErr
	}
	if len(data) != tile.size {
		return 0, 0, 0, errors.New("SQLite tile length differs from corpus")
	}
	return ready, lookup, total, nil
}

func measureColdRequestTinyTiles(artifactPath string, memory int64, tile benchmarkTile) (ready, lookup, total time.Duration, err error) {
	totalStart := time.Now()
	dataset, err := tinytiles.Open(context.Background(), artifactPath, tinytiles.OpenOptions{Readers: 1, MaxMemoryBytes: memory})
	if err != nil {
		return 0, 0, 0, err
	}
	ready = time.Since(totalStart)
	lookupStart := time.Now()
	value, found, err := dataset.LookupTMS(context.Background(), tiles.Key{Z: tile.z, X: tile.x, Y: tile.y})
	lookup = time.Since(lookupStart)
	total = time.Since(totalStart)
	closeErr := dataset.Close()
	if err != nil {
		return 0, 0, 0, err
	}
	if closeErr != nil {
		return 0, 0, 0, closeErr
	}
	if !found || len(value.Data) != tile.size {
		return 0, 0, 0, errors.New("tinyTiles tile length differs from corpus")
	}
	return ready, lookup, total, nil
}

func coldRequestStatsFromDurations(ready, lookup, total []time.Duration) (coldRequestStats, error) {
	readyStats, err := latencyStatsFromDurations(ready)
	if err != nil {
		return coldRequestStats{}, err
	}
	lookupStats, err := latencyStatsFromDurations(lookup)
	if err != nil {
		return coldRequestStats{}, err
	}
	totalStats, err := latencyStatsFromDurations(total)
	if err != nil {
		return coldRequestStats{}, err
	}
	return coldRequestStats{ready: readyStats, lookup: lookupStats, total: totalStats}, nil
}

func medianColdRequestStats(runs []coldRequestStats) (coldRequestStats, error) {
	if len(runs) == 0 {
		return coldRequestStats{}, errors.New("no application-cold runs to aggregate")
	}
	ready := make([]latencyStats, 0, len(runs))
	lookup := make([]latencyStats, 0, len(runs))
	total := make([]latencyStats, 0, len(runs))
	for _, run := range runs {
		ready = append(ready, run.ready)
		lookup = append(lookup, run.lookup)
		total = append(total, run.total)
	}
	readyMedian, err := medianLatencyStats(ready)
	if err != nil {
		return coldRequestStats{}, err
	}
	lookupMedian, err := medianLatencyStats(lookup)
	if err != nil {
		return coldRequestStats{}, err
	}
	totalMedian, err := medianLatencyStats(total)
	if err != nil {
		return coldRequestStats{}, err
	}
	return coldRequestStats{ready: readyMedian, lookup: lookupMedian, total: totalMedian}, nil
}

func benchmarkReaderCounts(limit int) []int {
	counts := []int{1}
	for _, readers := range []int{4, 8} {
		if readers <= limit {
			counts = append(counts, readers)
		}
	}
	return counts
}

// measureFreshReaderCorpus repeats every complete reader profile without
// changing the randomized, unique corpus. Each run gets new Dataset readers
// and new SQLite connections/statements, and the result is the median p50,
// p95 and p99 across those complete runs. This prevents a one-off scheduling
// outlier from deciding the cold p95 gate without removing any slow tiles.
//
// It is deliberately called after corpus selection and before sample parity,
// complete parity, or warm-up reads.
func measureFreshReaderCorpus(sourcePath, artifactPath string, source sqliteBenchmarkSource, corpus []benchmarkTile, memory int64, readerCounts []int, runs int) ([]freshReaderCorpusMeasurement, error) {
	if runs < 1 {
		return nil, errors.New("fresh-reader corpus runs must be positive")
	}
	measurements := make([]freshReaderCorpusMeasurement, 0, len(readerCounts))
	for _, readers := range readerCounts {
		sqliteRuns := make([]latencyStats, 0, runs)
		tinyRuns := make([]latencyStats, 0, runs)
		for run := 0; run < runs; run++ {
			sqliteStats, tinyStats, err := measureFreshReaderCorpusRun(sourcePath, artifactPath, source, corpus, memory, readers, run%2 == 0)
			if err != nil {
				return nil, fmt.Errorf("run %d/%d readers=%d: %w", run+1, runs, readers, err)
			}
			sqliteRuns = append(sqliteRuns, sqliteStats)
			tinyRuns = append(tinyRuns, tinyStats)
		}
		sqliteMedian, err := medianLatencyStats(sqliteRuns)
		if err != nil {
			return nil, fmt.Errorf("aggregate SQLite readers=%d: %w", readers, err)
		}
		tinyMedian, err := medianLatencyStats(tinyRuns)
		if err != nil {
			return nil, fmt.Errorf("aggregate artifact readers=%d: %w", readers, err)
		}
		measurements = append(measurements, freshReaderCorpusMeasurement{readers: readers, sqlite: sqliteMedian, tiny: tinyMedian})
	}
	return measurements, nil
}

// measureFreshReaderCorpusRun executes one complete profile using only fresh
// logical readers. It keeps the source corpus fixed so aggregation cannot
// suppress a slow coordinate by selecting a different sample. Successive
// profiles alternate the backend measurement order to avoid a fixed first or
// second position bias from filesystem activity.
func measureFreshReaderCorpusRun(sourcePath, artifactPath string, source sqliteBenchmarkSource, corpus []benchmarkTile, memory int64, readers int, sqliteFirst bool) (latencyStats, latencyStats, error) {
	sqliteReaders, err := openBenchmarkSQLiteReaders(sourcePath, source.lookupSQL, readers)
	if err != nil {
		return latencyStats{}, latencyStats{}, err
	}
	defer closeBenchmarkSQLiteReaders(sqliteReaders)
	dataset, err := tinytiles.Open(context.Background(), artifactPath, tinytiles.OpenOptions{Readers: readers, MaxMemoryBytes: memory})
	if err != nil {
		return latencyStats{}, latencyStats{}, fmt.Errorf("open artifact readers=%d: %w", readers, err)
	}
	datasetOpen := true
	defer func() {
		if datasetOpen {
			_ = dataset.Close()
		}
	}()

	sqliteLookup := func(reader int, tile benchmarkTile) error {
		var data []byte
		if err := sqliteReaders[reader].stmt.QueryRowContext(context.Background(), tile.z, tile.x, tile.y).Scan(&data); err != nil {
			return err
		}
		if len(data) != tile.size {
			return errors.New("SQLite tile length differs from corpus")
		}
		return nil
	}
	tinyLookup := func(_ int, tile benchmarkTile) error {
		value, found, err := dataset.LookupTMS(context.Background(), tiles.Key{Z: tile.z, X: tile.x, Y: tile.y})
		if err != nil {
			return err
		}
		if !found || len(value.Data) != tile.size {
			return errors.New("tinyTiles tile length differs from corpus")
		}
		return nil
	}
	var sqliteStats, tinyStats latencyStats
	if sqliteFirst {
		sqliteStats, err = measureParallelByReader(corpus, readers, sqliteLookup)
		if err != nil {
			return latencyStats{}, latencyStats{}, fmt.Errorf("SQLite readers=%d: %w", readers, err)
		}
		tinyStats, err = measureParallelByReader(corpus, readers, tinyLookup)
		if err != nil {
			return latencyStats{}, latencyStats{}, fmt.Errorf("artifact readers=%d: %w", readers, err)
		}
	} else {
		tinyStats, err = measureParallelByReader(corpus, readers, tinyLookup)
		if err != nil {
			return latencyStats{}, latencyStats{}, fmt.Errorf("artifact readers=%d: %w", readers, err)
		}
		sqliteStats, err = measureParallelByReader(corpus, readers, sqliteLookup)
		if err != nil {
			return latencyStats{}, latencyStats{}, fmt.Errorf("SQLite readers=%d: %w", readers, err)
		}
	}
	if err := dataset.Close(); err != nil {
		return latencyStats{}, latencyStats{}, fmt.Errorf("close artifact readers=%d: %w", readers, err)
	}
	datasetOpen = false
	return sqliteStats, tinyStats, nil
}

// medianLatencyStats aggregates each percentile independently. This is the
// statistic reported by --cold-runs and used by the cold p95 gate.
func medianLatencyStats(runs []latencyStats) (latencyStats, error) {
	if len(runs) == 0 {
		return latencyStats{}, errors.New("no latency runs to aggregate")
	}
	p50 := make([]time.Duration, 0, len(runs))
	p95 := make([]time.Duration, 0, len(runs))
	p99 := make([]time.Duration, 0, len(runs))
	for _, run := range runs {
		p50 = append(p50, run.p50)
		p95 = append(p95, run.p95)
		p99 = append(p99, run.p99)
	}
	return latencyStats{p50: medianDuration(p50), p95: medianDuration(p95), p99: medianDuration(p99)}, nil
}

func medianDuration(values []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	middle := len(sorted) / 2
	if len(sorted)%2 != 0 {
		return sorted[middle]
	}
	return sorted[middle-1] + (sorted[middle]-sorted[middle-1])/2
}

func coldP95Failures(measurements []freshReaderCorpusMeasurement, ratio float64) []coldP95Failure {
	var failures []coldP95Failure
	for _, measurement := range measurements {
		if float64(measurement.tiny.p95) > float64(measurement.sqlite.p95)*ratio {
			failures = append(failures, coldP95Failure{readers: measurement.readers, sqlite: measurement.sqlite.p95, tiny: measurement.tiny.p95})
		}
	}
	return failures
}

type sqliteBenchmarkSource struct {
	schema             tiles.Schema
	zoomLevelsSQL      string
	tileCountAtZoomSQL string
	tilesAtZoomSQL     string
	lookupSQL          string
	rangeSQL           string
	fullTilesSQL       string
}

func detectBenchmarkSource(db *sql.DB) (sqliteBenchmarkSource, error) {
	flat, err := sqliteTableExists(db, "tiles")
	if err != nil {
		return sqliteBenchmarkSource{}, err
	}
	if flat {
		return sqliteBenchmarkSource{
			schema:             tiles.SchemaFlat,
			zoomLevelsSQL:      `SELECT DISTINCT zoom_level FROM tiles ORDER BY zoom_level`,
			tileCountAtZoomSQL: `SELECT COUNT(*) FROM tiles WHERE zoom_level=?`,
			tilesAtZoomSQL:     `SELECT zoom_level,tile_column,tile_row,length(tile_data) FROM tiles WHERE zoom_level=? ORDER BY tile_column,tile_row`,
			lookupSQL:          `SELECT tile_data FROM tiles WHERE zoom_level=? AND tile_column=? AND tile_row=?`,
			rangeSQL:           `SELECT zoom_level,tile_column,tile_row,tile_data FROM tiles WHERE zoom_level=? AND tile_column BETWEEN ? AND ? AND tile_row BETWEEN ? AND ?`,
			fullTilesSQL:       `SELECT zoom_level,tile_column,tile_row,tile_data FROM tiles ORDER BY zoom_level,tile_column,tile_row`,
		}, nil
	}
	mapTable, err := sqliteTableExists(db, "map")
	if err != nil {
		return sqliteBenchmarkSource{}, err
	}
	images, err := sqliteTableExists(db, "images")
	if err != nil {
		return sqliteBenchmarkSource{}, err
	}
	if mapTable && images {
		return sqliteBenchmarkSource{
			schema:             tiles.SchemaNormalized,
			zoomLevelsSQL:      `SELECT DISTINCT zoom_level FROM map ORDER BY zoom_level`,
			tileCountAtZoomSQL: `SELECT COUNT(*) FROM map WHERE zoom_level=?`,
			tilesAtZoomSQL:     `SELECT m.zoom_level,m.tile_column,m.tile_row,length(i.tile_data) FROM map AS m JOIN images AS i ON i.tile_id=m.tile_id WHERE m.zoom_level=? ORDER BY m.tile_column,m.tile_row`,
			lookupSQL:          `SELECT i.tile_data FROM map AS m JOIN images AS i ON i.tile_id=m.tile_id WHERE m.zoom_level=? AND m.tile_column=? AND m.tile_row=?`,
			rangeSQL:           `SELECT m.zoom_level,m.tile_column,m.tile_row,i.tile_data FROM map AS m JOIN images AS i ON i.tile_id=m.tile_id WHERE m.zoom_level=? AND m.tile_column BETWEEN ? AND ? AND m.tile_row BETWEEN ? AND ?`,
			fullTilesSQL:       `SELECT m.zoom_level,m.tile_column,m.tile_row,i.tile_data FROM map AS m JOIN images AS i ON i.tile_id=m.tile_id ORDER BY m.zoom_level,m.tile_column,m.tile_row`,
		}, nil
	}
	return sqliteBenchmarkSource{}, errors.New("source has neither flat tiles nor normalized map/images schema")
}

func sqliteTableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect source schema: %w", err)
	}
	return true, nil
}

// benchmarkCorpus makes a reproducible, zoom-stratified corpus. It samples
// uniformly within each zoom and redistributes unused quota from sparse zooms,
// rather than taking the first coordinates at every zoom. That keeps a
// requested 512-request run at 512 requests whenever the source contains at
// least 512 tiles, including regional sources with a very sparse low zoom.
func benchmarkCorpus(db *sql.DB, source sqliteBenchmarkSource, count int, seed int64) ([]benchmarkTile, error) {
	levels, err := db.Query(source.zoomLevelsSQL)
	if err != nil {
		return nil, err
	}
	defer levels.Close()
	var zooms []int
	for levels.Next() {
		var z int
		if err := levels.Scan(&z); err != nil {
			return nil, err
		}
		zooms = append(zooms, z)
	}
	if err := levels.Err(); err != nil {
		return nil, err
	}
	if len(zooms) == 0 {
		return nil, errors.New("source has no tiles")
	}
	available := make([]int, len(zooms))
	totalAvailable := 0
	for index, z := range zooms {
		var tileCount int
		if err := db.QueryRow(source.tileCountAtZoomSQL, z).Scan(&tileCount); err != nil {
			return nil, fmt.Errorf("count source tiles at zoom %d: %w", z, err)
		}
		if tileCount < 0 {
			return nil, fmt.Errorf("source has invalid negative tile count at zoom %d", z)
		}
		available[index] = tileCount
		totalAvailable += tileCount
	}
	if totalAvailable == 0 {
		return nil, errors.New("source has no readable tiles")
	}
	target := min(count, totalAvailable)
	quotas := benchmarkZoomQuotas(available, target)
	corpus := make([]benchmarkTile, 0, target)
	rng := rand.New(rand.NewSource(seed ^ 0x6A09E667F3BCC909))
	for index, z := range zooms {
		if quotas[index] == 0 {
			continue
		}
		rows, err := db.Query(source.tilesAtZoomSQL, z)
		if err != nil {
			return nil, err
		}
		tilesAtZoom, err := reservoirBenchmarkTiles(rows, quotas[index], rng)
		if err != nil {
			return nil, fmt.Errorf("sample source tiles at zoom %d: %w", z, err)
		}
		corpus = append(corpus, tilesAtZoom...)
	}
	if len(corpus) != target {
		return nil, fmt.Errorf("sampled %d tiles, want %d", len(corpus), target)
	}
	return corpus, nil
}

// benchmarkZoomQuotas assigns an equal share to every available zoom first,
// then cycles through zooms with spare coordinates until the request budget is
// full. The difference between non-exhausted zoom quotas is at most one.
func benchmarkZoomQuotas(available []int, target int) []int {
	quotas := make([]int, len(available))
	remaining := target
	for remaining > 0 {
		progressed := false
		for index, count := range available {
			if quotas[index] >= count {
				continue
			}
			quotas[index]++
			remaining--
			progressed = true
			if remaining == 0 {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return quotas
}

// reservoirBenchmarkTiles samples a zoom without retaining every coordinate
// in memory. SQL's ordering makes the stream reproducible, while reservoir
// sampling avoids a spatially biased first-N slice.
func reservoirBenchmarkTiles(rows *sql.Rows, limit int, rng *rand.Rand) ([]benchmarkTile, error) {
	defer rows.Close()
	if limit < 1 {
		return nil, errors.New("reservoir limit must be positive")
	}
	selected := make([]benchmarkTile, 0, limit)
	seen := 0
	for rows.Next() {
		var tile benchmarkTile
		if err := rows.Scan(&tile.z, &tile.x, &tile.y, &tile.size); err != nil {
			return nil, err
		}
		if tile.size < 0 {
			return nil, fmt.Errorf("negative tile size at %d/%d/%d", tile.z, tile.x, tile.y)
		}
		seen++
		if len(selected) < limit {
			selected = append(selected, tile)
			continue
		}
		if replacement := rng.Intn(seen); replacement < limit {
			selected[replacement] = tile
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(selected) != limit {
		return nil, fmt.Errorf("sampled %d tiles from %d available rows, want %d", len(selected), seen, limit)
	}
	return selected, nil
}

// randomizeUniqueBenchmarkCorpus makes request order reproducible without
// allowing duplicate coordinate lookups to turn a reader-cache hit into a
// misleading "cold" sample. Duplicate MBTiles coordinate keys are invalid for
// this benchmark, so report them instead of silently changing the workload.
func randomizeUniqueBenchmarkCorpus(corpus []benchmarkTile, seed int64) error {
	seen := make(map[[3]int]struct{}, len(corpus))
	for _, tile := range corpus {
		key := [3]int{tile.z, tile.x, tile.y}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("benchmark corpus contains duplicate tile key %d/%d/%d", tile.z, tile.x, tile.y)
		}
		seen[key] = struct{}{}
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(corpus), func(i, j int) { corpus[i], corpus[j] = corpus[j], corpus[i] })
	return nil
}

func latencyStatsFromDurations(values []time.Duration) (latencyStats, error) {
	if len(values) == 0 {
		return latencyStats{}, errors.New("latency sample must not be empty")
	}
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return latencyStats{
		p50: ordered[(len(ordered)-1)*50/100],
		p95: ordered[(len(ordered)-1)*95/100],
		p99: ordered[(len(ordered)-1)*99/100],
	}, nil
}

func warm(corpus []benchmarkTile, lookup func(benchmarkTile) error) error {
	for _, tile := range corpus {
		if err := lookup(tile); err != nil {
			return err
		}
	}
	return nil
}
func measure(corpus []benchmarkTile, lookup func(benchmarkTile) error) (latencyStats, error) {
	latencies := make([]time.Duration, 0, len(corpus))
	for _, tile := range corpus {
		start := time.Now()
		if err := lookup(tile); err != nil {
			return latencyStats{}, err
		}
		latencies = append(latencies, time.Since(start))
	}
	return latencyStatsFromDurations(latencies)
}

func measureParallel(corpus []benchmarkTile, workers int, lookup func(benchmarkTile) error) (latencyStats, error) {
	if workers < 1 {
		return latencyStats{}, errors.New("parallel worker count must be positive")
	}
	type job struct {
		index int
		tile  benchmarkTile
	}
	jobs := make(chan job, workers*2)
	latencies := make([]time.Duration, len(corpus))
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for current := range jobs {
				start := time.Now()
				if err := lookup(current.tile); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				latencies[current.index] = time.Since(start)
			}
		}()
	}
	for index, tile := range corpus {
		jobs <- job{index: index, tile: tile}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return latencyStats{}, firstErr
	}
	return latencyStatsFromDurations(latencies)
}

// measureParallelByReader pins a worker to its supplied reader index. This is
// useful for cold measurements: every worker receives a separate fresh SQLite
// page cache and statement, while the Dataset keeps the same independent-reader
// pool shape that it uses in production.
func measureParallelByReader(corpus []benchmarkTile, workers int, lookup func(reader int, tile benchmarkTile) error) (latencyStats, error) {
	if workers < 1 {
		return latencyStats{}, errors.New("parallel worker count must be positive")
	}
	if len(corpus) == 0 {
		return latencyStats{}, errors.New("parallel benchmark corpus must not be empty")
	}
	latencies := make([]time.Duration, len(corpus))
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			<-start
			for index := reader; index < len(corpus); index += workers {
				start := time.Now()
				if err := lookup(reader, corpus[index]); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				latencies[index] = time.Since(start)
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	if firstErr != nil {
		return latencyStats{}, firstErr
	}
	return latencyStatsFromDurations(latencies)
}

func verifyBenchmarkParity(corpus []benchmarkTile, stmt *sql.Stmt, dataset *tinytiles.Dataset) error {
	ctx := context.Background()
	for _, request := range corpus {
		var sqliteData []byte
		if err := stmt.QueryRowContext(ctx, request.z, request.x, request.y).Scan(&sqliteData); err != nil {
			return err
		}
		tile, found, err := dataset.LookupTMS(ctx, tiles.Key{Z: request.z, X: request.x, Y: request.y})
		if err != nil {
			return err
		}
		if !found || !bytes.Equal(sqliteData, tile.Data) {
			return fmt.Errorf("tile bytes differ at %d/%d/%d", request.z, request.x, request.y)
		}
	}
	return nil
}

func verifyFullBenchmarkParity(db *sql.DB, source sqliteBenchmarkSource, info tiles.ArtifactInfo) (int64, int64, error) {
	rows, err := db.Query(source.fullTilesSQL)
	if err != nil {
		return 0, 0, err
	}
	tileHash := sha256.New()
	var tileCount int64
	var previous [3]int
	for rows.Next() {
		var z, x, y int
		var data []byte
		if err := rows.Scan(&z, &x, &y, &data); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		key := [3]int{z, x, y}
		if tileCount > 0 && key == previous {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("duplicate source tile key %d/%d/%d", z, x, y)
		}
		previous = key
		hashBenchmarkTile(tileHash, z, x, y, data)
		tileCount++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	if got := hex.EncodeToString(tileHash.Sum(nil)); got != info.TileDigestSHA256 {
		return 0, 0, fmt.Errorf("complete source tile digest %s differs from artifact %s", got, info.TileDigestSHA256)
	}
	artifactTileRows := artifactRows(info, "tiles")
	if info.Schema == tiles.SchemaNormalized {
		artifactTileRows = artifactRows(info, "map")
	}
	if artifactTileRows != tileCount {
		return 0, 0, fmt.Errorf("source has %d tile keys, artifact declares %d", tileCount, artifactTileRows)
	}

	metadataHash := sha256.New()
	var metadataCount int64
	metadataExists, err := sqliteTableExists(db, "metadata")
	if err != nil {
		return 0, 0, err
	}
	if metadataExists {
		metadataRows, err := db.Query(`SELECT name,value FROM metadata ORDER BY name`)
		if err != nil {
			return 0, 0, err
		}
		var previousName string
		for metadataRows.Next() {
			var name, value sql.NullString
			if err := metadataRows.Scan(&name, &value); err != nil {
				_ = metadataRows.Close()
				return 0, 0, err
			}
			if !name.Valid || !value.Valid {
				_ = metadataRows.Close()
				return 0, 0, errors.New("source metadata contains NULL name/value")
			}
			if metadataCount > 0 && name.String == previousName {
				_ = metadataRows.Close()
				return 0, 0, fmt.Errorf("duplicate source metadata name %q", name.String)
			}
			previousName = name.String
			hashBenchmarkMetadata(metadataHash, name.String, value.String)
			metadataCount++
		}
		if err := metadataRows.Err(); err != nil {
			_ = metadataRows.Close()
			return 0, 0, err
		}
		if err := metadataRows.Close(); err != nil {
			return 0, 0, err
		}
	}
	if got := hex.EncodeToString(metadataHash.Sum(nil)); got != info.MetadataDigestSHA256 {
		return 0, 0, fmt.Errorf("complete source metadata digest %s differs from artifact %s", got, info.MetadataDigestSHA256)
	}
	if artifactRows(info, "metadata") != metadataCount {
		return 0, 0, fmt.Errorf("source has %d metadata rows, artifact declares %d", metadataCount, artifactRows(info, "metadata"))
	}
	return tileCount, metadataCount, nil
}

func artifactRows(info tiles.ArtifactInfo, name string) int64 {
	for _, table := range info.Tables {
		if table.Name == name {
			return table.Rows
		}
	}
	return -1
}

func hashBenchmarkTile(dst io.Writer, z, x, y int, data []byte) {
	var buffer [8]byte
	for _, value := range []int{z, x, y} {
		binary.BigEndian.PutUint64(buffer[:], uint64(int64(value)))
		_, _ = dst.Write(buffer[:])
	}
	binary.BigEndian.PutUint64(buffer[:], uint64(len(data)))
	_, _ = dst.Write(buffer[:])
	_, _ = dst.Write(data)
}

func hashBenchmarkMetadata(dst io.Writer, name, value string) {
	var buffer [4]byte
	binary.BigEndian.PutUint32(buffer[:], uint32(len(name)))
	_, _ = dst.Write(buffer[:])
	_, _ = io.WriteString(dst, name)
	binary.BigEndian.PutUint32(buffer[:], uint32(len(value)))
	_, _ = dst.Write(buffer[:])
	_, _ = io.WriteString(dst, value)
}

func sqliteRangeLookup(stmt *sql.Stmt) func(benchmarkTile) error {
	return func(request benchmarkTile) error {
		xMax, yMax := benchmarkRange(request)
		rows, err := stmt.Query(request.z, request.x, xMax, request.y, yMax)
		if err != nil {
			return err
		}
		defer rows.Close()
		matched := false
		for rows.Next() {
			var z, x, y int
			var data []byte
			if err := rows.Scan(&z, &x, &y, &data); err != nil {
				return err
			}
			if z == request.z && x == request.x && y == request.y && len(data) == request.size {
				matched = true
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !matched {
			return errors.New("SQLite spatial lookup omitted requested tile")
		}
		return nil
	}
}

func tinyRangeLookup(dataset *tinytiles.Dataset) func(benchmarkTile) error {
	return func(request benchmarkTile) error {
		xMax, yMax := benchmarkRange(request)
		matched := false
		err := dataset.ScanTMS(context.Background(), tiles.Range{Z: request.z, XMin: request.x, XMax: xMax, YMin: request.y, YMax: yMax}, func(tile tiles.Tile) error {
			if tile.Key.Z == request.z && tile.Key.X == request.x && tile.Key.Y == request.y && len(tile.Data) == request.size {
				matched = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !matched {
			return errors.New("tinyTiles spatial lookup omitted requested tile")
		}
		return nil
	}
}

func benchmarkRange(tile benchmarkTile) (xMax, yMax int) {
	limit := 1 << tile.z
	xMax, yMax = tile.x+1, tile.y+1
	if xMax >= limit {
		xMax = limit - 1
	}
	if yMax >= limit {
		yMax = limit - 1
	}
	return xMax, yMax
}

func benchmarkSizes(source, artifact string) (int64, int64, error) {
	info, err := os.Stat(source)
	if err != nil {
		return 0, 0, err
	}
	var artifactBytes int64
	err = filepath.WalkDir(artifact, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			fileInfo, err := entry.Info()
			if err != nil {
				return err
			}
			artifactBytes += fileInfo.Size()
		}
		return nil
	})
	return info.Size(), artifactBytes, err
}

func printLatency(output io.Writer, workload, backend string, readers int, stats latencyStats) {
	fmt.Fprintf(output, "%s\t%s\t%d\t%s\t%s\t%s\n", workload, backend, readers, stats.p50, stats.p95, stats.p99)
}
