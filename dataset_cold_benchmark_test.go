//go:build sqliteimport && !js && !wasm && !baremetal

package tinytiles

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/SimonWaldherr/tinySQL/importer"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

const (
	coldBenchmarkSourceEnv   = "TINYTILES_COLD_BENCH_SOURCE"
	coldBenchmarkArtifactEnv = "TINYTILES_COLD_BENCH_ARTIFACT"
	coldBenchmarkSamplesEnv  = "TINYTILES_COLD_BENCH_SAMPLES"
	coldBenchmarkMemoryEnv   = "TINYTILES_COLD_BENCH_MEMORY"
)

// TestArtifactColdFirstLookupBenchmark measures the request path before any
// tile lookup has warmed the application's SQLite or tinyTiles reader cache.
// It is opt-in because it needs a real MBTiles/artifact pair supplied through
// environment variables. It intentionally keeps the OS page cache untouched:
// that needs a separate, controlled host-level procedure.
//
// Before timing, the artifact is strictly validated once. That mirrors the
// production lifecycle (validate before publish), makes each following
// Dataset.Open create a fresh reader rather than repeat integrity auditing, and
// lets the test measure a cold request rather than deployment validation.
func TestArtifactColdFirstLookupBenchmark(t *testing.T) {
	source := strings.TrimSpace(os.Getenv(coldBenchmarkSourceEnv))
	artifact := strings.TrimSpace(os.Getenv(coldBenchmarkArtifactEnv))
	if source == "" || artifact == "" {
		t.Skipf("set %s and %s to run the real-artifact cold reader benchmark", coldBenchmarkSourceEnv, coldBenchmarkArtifactEnv)
	}
	samples, err := coldBenchmarkPositiveEnv(coldBenchmarkSamplesEnv, 512)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := coldBenchmarkPositiveInt64Env(coldBenchmarkMemoryEnv, 64<<20)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	keys, sourceSchema, err := coldBenchmarkKeys(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("cold benchmark source has no tile keys")
	}
	rand.New(rand.NewSource(0xB3A11)).Shuffle(len(keys), func(i, j int) {
		keys[i], keys[j] = keys[j], keys[i]
	})
	if samples > len(keys) {
		samples = len(keys)
	}
	keys = keys[:samples]

	// This uses a short-lived validation reader. Every timed Dataset.Open below
	// still creates a fresh storage reader and fresh page cache.
	if _, err := tiles.ValidateArtifact(ctx, artifact); err != nil {
		t.Fatalf("prevalidate artifact: %v", err)
	}

	var sqliteStats, tinyStats coldBenchmarkStats
	for index, key := range keys {
		// Interleave the backends to avoid giving one of them all of the earlier
		// scheduler/thermal state. Each operation gets a newly opened client.
		if index%2 == 0 {
			if err := coldBenchmarkSQLite(ctx, source, sourceSchema, key, &sqliteStats); err != nil {
				t.Fatal(err)
			}
			if err := coldBenchmarkTinyTiles(ctx, artifact, memory, key, &tinyStats); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := coldBenchmarkTinyTiles(ctx, artifact, memory, key, &tinyStats); err != nil {
			t.Fatal(err)
		}
		if err := coldBenchmarkSQLite(ctx, source, sourceSchema, key, &sqliteStats); err != nil {
			t.Fatal(err)
		}
	}

	t.Logf("application-cold source=%q artifact=%q requests=%d seed=0xB3A11 per-reader-memory=%d", source, artifact, len(keys), memory)
	t.Logf("backend\tready-p50\tready-p95\tlookup-p50\tlookup-p95\tlookup-p99\ttotal-p50\ttotal-p95\tbytes")
	t.Logf("SQLite\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d", sqliteStats.ready.p50(), sqliteStats.ready.p95(), sqliteStats.lookup.p50(), sqliteStats.lookup.p95(), sqliteStats.lookup.p99(), sqliteStats.total.p50(), sqliteStats.total.p95(), sqliteStats.bytes)
	t.Logf("tinyTiles\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d", tinyStats.ready.p50(), tinyStats.ready.p95(), tinyStats.lookup.p50(), tinyStats.lookup.p95(), tinyStats.lookup.p99(), tinyStats.total.p50(), tinyStats.total.p95(), tinyStats.bytes)
}

type coldBenchmarkSchema string

const (
	coldBenchmarkFlat       coldBenchmarkSchema = "flat"
	coldBenchmarkNormalized coldBenchmarkSchema = "normalized"
)

func coldBenchmarkKeys(ctx context.Context, source string) ([]tiles.Key, coldBenchmarkSchema, error) {
	db, err := sql.Open("sqlite", coldBenchmarkSQLiteDSN(source))
	if err != nil {
		return nil, "", fmt.Errorf("open cold benchmark source: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, "", fmt.Errorf("ping cold benchmark source: %w", err)
	}
	flat, err := coldBenchmarkTableExists(ctx, db, "tiles")
	if err != nil {
		return nil, "", err
	}
	schema := coldBenchmarkNormalized
	query := `SELECT zoom_level, tile_column, tile_row FROM map ORDER BY zoom_level, tile_column, tile_row`
	if flat {
		schema = coldBenchmarkFlat
		query = `SELECT zoom_level, tile_column, tile_row FROM tiles ORDER BY zoom_level, tile_column, tile_row`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, "", fmt.Errorf("read cold benchmark tile keys: %w", err)
	}
	defer rows.Close()
	keys := make([]tiles.Key, 0, 1024)
	for rows.Next() {
		var key tiles.Key
		if err := rows.Scan(&key.Z, &key.X, &key.Y); err != nil {
			return nil, "", fmt.Errorf("scan cold benchmark tile key: %w", err)
		}
		if err := key.Validate(); err != nil {
			return nil, "", fmt.Errorf("invalid cold benchmark tile key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate cold benchmark tile keys: %w", err)
	}
	return keys, schema, nil
}

func coldBenchmarkTableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("inspect cold benchmark source schema: %w", err)
}

func coldBenchmarkSQLite(ctx context.Context, source string, schema coldBenchmarkSchema, key tiles.Key, stats *coldBenchmarkStats) error {
	totalStart := time.Now()
	readyStart := totalStart
	db, err := sql.Open("sqlite", coldBenchmarkSQLiteDSN(source))
	if err != nil {
		return fmt.Errorf("open cold SQLite reader: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping cold SQLite reader: %w", err)
	}
	ready := time.Since(readyStart)
	lookupStart := time.Now()
	query := `SELECT tile_data FROM tiles WHERE zoom_level=? AND tile_column=? AND tile_row=?`
	if schema == coldBenchmarkNormalized {
		query = `SELECT images.tile_data FROM map JOIN images USING(tile_id) WHERE map.zoom_level=? AND map.tile_column=? AND map.tile_row=?`
	}
	var data []byte
	if err := db.QueryRowContext(ctx, query, key.Z, key.X, key.Y).Scan(&data); err != nil {
		return fmt.Errorf("cold SQLite lookup %d/%d/%d: %w", key.Z, key.X, key.Y, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("cold SQLite lookup %d/%d/%d returned an empty tile", key.Z, key.X, key.Y)
	}
	stats.add(ready, time.Since(lookupStart), time.Since(totalStart), len(data))
	return nil
}

// coldBenchmarkSQLiteDSN makes each benchmark client use a private SQLite
// page cache. Without cache=private, two short-lived connections may share a
// cache depending on the driver's URI defaults, which would invalidate a
// per-request application-cold comparison.
func coldBenchmarkSQLiteDSN(source string) string {
	return "file:" + filepath.Clean(source) + "?mode=ro&immutable=1&cache=private"
}

func coldBenchmarkTinyTiles(ctx context.Context, artifact string, memory int64, key tiles.Key, stats *coldBenchmarkStats) error {
	totalStart := time.Now()
	dataset, err := Open(ctx, artifact, OpenOptions{Readers: 1, MaxMemoryBytes: memory})
	if err != nil {
		return fmt.Errorf("open cold tinyTiles reader: %w", err)
	}
	ready := time.Since(totalStart)
	lookupStart := time.Now()
	tile, found, err := dataset.LookupTMS(ctx, key)
	lookup := time.Since(lookupStart)
	total := time.Since(totalStart)
	closeErr := dataset.Close()
	if err != nil {
		return fmt.Errorf("cold tinyTiles lookup %d/%d/%d: %w", key.Z, key.X, key.Y, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close cold tinyTiles reader: %w", closeErr)
	}
	if !found || len(tile.Data) == 0 {
		return fmt.Errorf("cold tinyTiles lookup %d/%d/%d found=%t bytes=%d", key.Z, key.X, key.Y, found, len(tile.Data))
	}
	stats.add(ready, lookup, total, len(tile.Data))
	return nil
}

type coldBenchmarkStats struct {
	ready, lookup, total coldBenchmarkDurations
	bytes                int64
}

func (s *coldBenchmarkStats) add(ready, lookup, total time.Duration, bytes int) {
	s.ready = append(s.ready, ready)
	s.lookup = append(s.lookup, lookup)
	s.total = append(s.total, total)
	s.bytes += int64(bytes)
}

type coldBenchmarkDurations []time.Duration

func (values coldBenchmarkDurations) p50() time.Duration { return values.percentile(50) }
func (values coldBenchmarkDurations) p95() time.Duration { return values.percentile(95) }
func (values coldBenchmarkDurations) p99() time.Duration { return values.percentile(99) }

func (values coldBenchmarkDurations) percentile(percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	ordered := append(coldBenchmarkDurations(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[(len(ordered)-1)*percent/100]
}

func coldBenchmarkPositiveEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func coldBenchmarkPositiveInt64Env(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive byte count", name)
	}
	return value, nil
}
