package server

import (
	"bytes"
	"sync"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestTileCacheRetainsRecentEntriesAndEvictsOldest(t *testing.T) {
	// Each normal shard gets 3 KiB. With the entry accounting below, two 1 KiB
	// payloads fit but the third requires an eviction from the same shard.
	cache := newTileCache(tileCacheShardCount * 4 << 10)
	first := tiles.Key{Z: 8, X: 0, Y: 0}
	keys := sameShardKeys(cache, first, 3)
	payload := bytes.Repeat([]byte{1}, 1<<10)
	for _, key := range keys[:2] {
		cache.put(key, payload, "checksum")
	}
	if _, _, found := cache.get(keys[0]); !found {
		t.Fatal("first entry missing before LRU touch")
	}
	cache.put(keys[2], payload, "checksum")
	if _, _, found := cache.get(keys[1]); found {
		t.Fatal("least recently used entry was retained")
	}
	if checksum, found := cache.checksum(keys[1]); !found || checksum != "checksum" {
		t.Fatalf("evicted payload checksum = %q found=%t", checksum, found)
	}
	if data, checksum, found := cache.get(keys[0]); !found || len(data) != len(payload) || checksum != "checksum" {
		t.Fatalf("recent entry = bytes=%d checksum=%q found=%t", len(data), checksum, found)
	}
	if _, _, found := cache.get(keys[2]); !found {
		t.Fatal("new entry missing")
	}
	if newTileCache(0) != nil || newTileCache(-1) != nil {
		t.Fatal("non-positive cache budget should disable the cache")
	}
}

func TestTileCacheUsesLargeTileLane(t *testing.T) {
	cache := newTileCache(8 << 20)
	data := bytes.Repeat([]byte{3}, 1<<20)
	key := tiles.Key{Z: 8, X: 1, Y: 1}
	cache.put(key, data, "checksum")
	if got, _, found := cache.get(key); !found || len(got) != len(data) {
		t.Fatalf("large cache entry = bytes=%d found=%t", len(got), found)
	}
}

func TestTileCacheRetainsChecksumForUncacheablePayload(t *testing.T) {
	cache := newTileCache(tileCacheShardCount * 4 << 10)
	key := tiles.Key{Z: 8, X: 1, Y: 1}
	cache.put(key, bytes.Repeat([]byte{3}, 64<<10), "checksum")
	if _, _, found := cache.get(key); found {
		t.Fatal("uncacheable payload was retained")
	}
	if checksum, found := cache.checksum(key); !found || checksum != "checksum" {
		t.Fatalf("uncacheable payload checksum = %q found=%t", checksum, found)
	}
}

func TestTileCacheFastFrontDoesNotRetainEvictedPayload(t *testing.T) {
	// One 1 KiB payload fits in a normal shard but two do not. The first get
	// publishes its immutable lock-free front snapshot; inserting the second
	// key must invalidate that snapshot once the LRU evicts it.
	cache := newTileCache(tileCacheShardCount * 2 << 10)
	keys := sameShardKeys(cache, tiles.Key{Z: 8, X: 0, Y: 0}, 2)
	payload := bytes.Repeat([]byte{1}, 1<<10)
	cache.put(keys[0], payload, "first")
	if _, _, found := cache.get(keys[0]); !found {
		t.Fatal("first entry missing before eviction")
	}
	cache.put(keys[1], payload, "second")
	if _, _, found := cache.get(keys[0]); found {
		t.Fatal("evicted payload remained visible through fast front")
	}
	if data, checksum, found := cache.get(keys[1]); !found || len(data) != len(payload) || checksum != "second" {
		t.Fatalf("new payload = bytes=%d checksum=%q found=%t", len(data), checksum, found)
	}
	assertTileCacheShardBound(t, cache.shard(keys[0]))
}

func TestTileCacheByteBoundIncludesFastFront(t *testing.T) {
	cache := newTileCache(tileCacheShardCount * 4 << 10)
	keys := sameShardKeys(cache, tiles.Key{Z: 8, X: 0, Y: 0}, 6)
	payload := bytes.Repeat([]byte{1}, 1<<10)
	for _, key := range keys {
		cache.put(key, payload, "checksum")
		if _, _, found := cache.get(key); !found {
			t.Fatalf("new payload %v missing", key)
		}
	}
	assertTileCacheShardBound(t, cache.shard(keys[0]))
}

func TestTileCacheConcurrentAccess(t *testing.T) {
	cache := newTileCache(8 << 20)
	data := bytes.Repeat([]byte{2}, 8<<10)
	keys := make([]tiles.Key, 64)
	for index := range keys {
		keys[index] = tiles.Key{Z: 8, X: index % 16, Y: index / 16}
	}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 500; iteration++ {
				key := keys[(worker+iteration)%len(keys)]
				cache.put(key, data, "checksum")
				if got, checksum, found := cache.get(key); !found || len(got) != len(data) || checksum != "checksum" {
					t.Errorf("cache entry = bytes=%d checksum=%q found=%t", len(got), checksum, found)
					return
				}
			}
		}()
	}
	group.Wait()
}

func BenchmarkTileCacheGetParallel(b *testing.B) {
	cache := newTileCache(DefaultTileCacheBytes)
	key := tiles.Key{Z: 8, X: 137, Y: 167}
	data := bytes.Repeat([]byte{1}, 64<<10)
	cache.put(key, data, "checksum")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			got, _, found := benchmarkTileCacheGet(cache, key)
			if !found || len(got) != len(data) {
				b.Fatalf("cache hit = bytes=%d found=%t", len(got), found)
			}
		}
	})
}

//go:noinline
func benchmarkTileCacheGet(cache *tileCache, key tiles.Key) ([]byte, string, bool) {
	// Keep the compiler from specializing the benchmark around a locally
	// initialized cache. A production Server receives cache state from other
	// goroutines, so the hot front load must be measured as a real operation.
	return cache.get(key)
}

func sameShardKeys(cache *tileCache, first tiles.Key, count int) []tiles.Key {
	keys := []tiles.Key{first}
	target := cache.shard(first)
	for x := 0; len(keys) < count; x++ {
		for y := 0; y < 1<<8 && len(keys) < count; y++ {
			key := tiles.Key{Z: 8, X: x, Y: y}
			if key == first || cache.shard(key) != target {
				continue
			}
			keys = append(keys, key)
		}
	}
	return keys
}

func assertTileCacheShardBound(t *testing.T, shard *tileCacheShard) {
	t.Helper()
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.used > shard.maxBytes {
		t.Fatalf("cache bytes = %d, maximum = %d", shard.used, shard.maxBytes)
	}
	var counted int64
	front := shard.front.Load()
	frontOwned := front == nil
	for _, element := range shard.entries {
		entry := element.Value.(*tileCacheEntry)
		counted += entry.size
		if entry.value != nil && entry.value.data == nil {
			t.Fatalf("entry %v data state is inconsistent", entry.key)
		}
		if entry.value == front {
			frontOwned = true
		}
	}
	if counted != shard.used {
		t.Fatalf("cache accounting = %d, counted = %d", shard.used, counted)
	}
	if !frontOwned {
		t.Fatal("fast front is not a retained payload entry")
	}
}
