package tinytiles

import (
	"context"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// benchmarkReader isolates Dataset's pool, validation and coordinate-boundary
// cost from pager I/O. Artifact benchmarks remain responsible for measuring
// real storage latency.
type benchmarkReader struct{}

func (benchmarkReader) Close() error { return nil }

func (benchmarkReader) Info() tiles.ArtifactInfo { return tiles.ArtifactInfo{} }

func (benchmarkReader) Metadata(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (benchmarkReader) Lookup(_ context.Context, key tiles.Key) (tiles.Tile, bool, error) {
	return tiles.Tile{Key: key, Data: benchmarkTileData}, true, nil
}

func (benchmarkReader) LookupFunc(ctx context.Context, key tiles.Key, fn func(tiles.Tile) error) (bool, error) {
	tile, found, err := benchmarkReader{}.Lookup(ctx, key)
	if err != nil || !found {
		return found, err
	}
	return true, fn(tile)
}

func (benchmarkReader) Scan(context.Context, tiles.Range, func(tiles.Tile) error) error { return nil }

var benchmarkTileData = []byte{1, 2, 3}

func newBenchmarkDataset() *Dataset {
	dataset := &Dataset{readers: make(chan tiles.Reader, 1), done: make(chan struct{})}
	dataset.readers <- benchmarkReader{}
	return dataset
}

func BenchmarkDatasetLookupTMS(b *testing.B) {
	dataset := newBenchmarkDataset()
	b.Cleanup(func() { _ = dataset.Close() })
	ctx := context.Background()
	key := tiles.Key{Z: 14, X: 8872, Y: 5372}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, found, err := dataset.LookupTMS(ctx, key); err != nil || !found {
			b.Fatalf("LookupTMS found=%t err=%v", found, err)
		}
	}
}

func BenchmarkDatasetLookupXYZ(b *testing.B) {
	dataset := newBenchmarkDataset()
	b.Cleanup(func() { _ = dataset.Close() })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, found, err := dataset.LookupXYZ(ctx, 14, 8872, 11011); err != nil || !found {
			b.Fatalf("LookupXYZ found=%t err=%v", found, err)
		}
	}
}
