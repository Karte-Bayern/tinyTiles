//go:build !js || !wasm

package offline

import (
	"context"
	"path/filepath"
	"testing"
)

func BenchmarkMemoryStoreGetTileParallel(b *testing.B) {
	store := NewMemoryStore()
	key := TileKey{Z: 8, X: 137, Y: 167}
	tile := checkedTile(makeBenchmarkTileBytes(8 << 10))
	if err := store.PutTile(context.Background(), "bench", "r1", key, tile); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			got, found, err := store.GetTile(context.Background(), "bench", "r1", key)
			if err != nil || !found || len(got.Data) != len(tile.Data) {
				b.Fatalf("get found=%t bytes=%d err=%v", found, len(got.Data), err)
			}
		}
	})
}

func BenchmarkFileStoreGetTileParallel(b *testing.B) {
	store, err := NewFileStore(filepath.Join(b.TempDir(), "cache"))
	if err != nil {
		b.Fatal(err)
	}
	key := TileKey{Z: 8, X: 137, Y: 167}
	tile := checkedTile(makeBenchmarkTileBytes(8 << 10))
	if err := store.PutTile(context.Background(), "bench", "r1", key, tile); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			got, found, err := store.GetTile(context.Background(), "bench", "r1", key)
			if err != nil || !found || len(got.Data) != len(tile.Data) {
				b.Fatalf("get found=%t bytes=%d err=%v", found, len(got.Data), err)
			}
		}
	})
}

func BenchmarkFileStorePutTileParallel(b *testing.B) {
	store, err := NewFileStore(filepath.Join(b.TempDir(), "cache"))
	if err != nil {
		b.Fatal(err)
	}
	key := TileKey{Z: 8, X: 137, Y: 167}
	tile := checkedTile(makeBenchmarkTileBytes(8 << 10))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if err := store.PutTile(context.Background(), "bench", "r1", key, tile); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSynchronizerStreamsRange(b *testing.B) {
	keys := []TileRange{{Z: 6, XMin: 0, XMax: 7, YMin: 0, YMax: 7}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store := NewMemoryStore()
		fetcher := &fakeFetcher{manifest: testManifest("benchmark")}
		synchronizer := &Synchronizer{Store: store, Fetcher: fetcher}
		result, err := synchronizer.Sync(context.Background(), SyncRequest{Ranges: keys, Concurrency: 4})
		if err != nil || result.Downloaded != 64 {
			b.Fatalf("sync result=%#v err=%v", result, err)
		}
	}
}

func BenchmarkSynchronizerReusesMemoryRange(b *testing.B) {
	request := SyncRequest{Ranges: []TileRange{{Z: 6, XMin: 0, XMax: 7, YMin: 0, YMax: 7}}, Concurrency: 4}
	for _, benchmark := range []struct {
		name string
		wrap bool
	}{
		{name: "built_in_store"},
		{name: "generic_store", wrap: true},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			store := NewMemoryStore()
			overrides := make(map[string]Tile, 64)
			largeTile := checkedTile(makeBenchmarkTileBytes(8 << 10))
			if err := request.visit(context.Background(), func(key TileKey) error {
				overrides[key.String()] = largeTile
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			fetcher := &fakeFetcher{manifest: testManifest("warm-benchmark"), override: overrides}
			warm := &Synchronizer{Store: store, Fetcher: fetcher}
			if _, err := warm.Sync(context.Background(), request); err != nil {
				b.Fatal(err)
			}
			var syncStore Store = store
			if benchmark.wrap {
				syncStore = benchmarkStoreWrapper{Store: store}
			}
			synchronizer := &Synchronizer{Store: syncStore, Fetcher: fetcher}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := synchronizer.Sync(context.Background(), request)
				if err != nil || result.Reused != 64 || result.Downloaded != 0 {
					b.Fatalf("sync result=%#v err=%v", result, err)
				}
			}
		})
	}
}

func BenchmarkSynchronizerReusesFileRange(b *testing.B) {
	request := SyncRequest{Ranges: []TileRange{{Z: 6, XMin: 0, XMax: 3, YMin: 0, YMax: 3}}, Concurrency: 4}
	for _, benchmark := range []struct {
		name string
		wrap bool
	}{
		{name: "built_in_store"},
		{name: "generic_store", wrap: true},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			store, err := NewFileStore(filepath.Join(b.TempDir(), "cache"))
			if err != nil {
				b.Fatal(err)
			}
			overrides := make(map[string]Tile, 16)
			largeTile := checkedTile(makeBenchmarkTileBytes(8 << 10))
			if err := request.visit(context.Background(), func(key TileKey) error {
				overrides[key.String()] = largeTile
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			fetcher := &fakeFetcher{manifest: testManifest("file-warm-benchmark"), override: overrides}
			warm := &Synchronizer{Store: store, Fetcher: fetcher}
			if _, err := warm.Sync(context.Background(), request); err != nil {
				b.Fatal(err)
			}
			var syncStore Store = store
			if benchmark.wrap {
				syncStore = benchmarkStoreWrapper{Store: store}
			}
			synchronizer := &Synchronizer{Store: syncStore, Fetcher: fetcher}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := synchronizer.Sync(context.Background(), request)
				if err != nil || result.Reused != 16 || result.Downloaded != 0 {
					b.Fatalf("sync result=%#v err=%v", result, err)
				}
			}
		})
	}
}

// benchmarkStoreWrapper deliberately hides the concrete implementation, so
// the generic synchronizer path remains measurable against its built-in fast
// path without changing public Store semantics.
type benchmarkStoreWrapper struct{ Store }

func makeBenchmarkTileBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	return data
}
