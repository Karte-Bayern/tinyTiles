package server

import (
	"container/list"
	"sync"
	"sync/atomic"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

const (
	// DefaultTileCacheBytes is the bounded in-process cache used by Server for
	// immutable tile payloads. It keeps frequently requested tiles out of both
	// the artifact pager and the per-response SHA-256 calculation.
	DefaultTileCacheBytes int64 = 32 << 20

	tileCacheShardCount = 16
	// Keep the byte accounting conservative for the list node, map entry and
	// immutable front snapshot in addition to the payload and checksum bytes.
	tileCacheEntryOverhead = 320
	tileCacheLargeFraction = 4
)

// tileCache is a sharded, byte-bounded LRU. Tile artifacts are immutable for
// the lifetime of a Server, so entries never need explicit invalidation.
// Payloads come from tiles.Reader as caller-owned slices; Server becomes their
// sole owner and can retain them without an additional copy.
type tileCache struct {
	shards    [tileCacheShardCount]tileCacheShard
	largeTile tileCacheShard
	hits      atomic.Int64
	misses    atomic.Int64
	puts      atomic.Int64
}

// tileCacheStats is a point-in-time snapshot of tileCache activity and
// occupancy, exposed for diagnostics (see Server's /stats endpoint).
type tileCacheStats struct {
	Hits      int64
	Misses    int64
	Puts      int64
	UsedBytes int64
	MaxBytes  int64
}

// Stats returns a snapshot of this cache's hit/miss counters and current
// byte occupancy. It is safe to call concurrently with get/put.
func (c *tileCache) Stats() tileCacheStats {
	if c == nil {
		return tileCacheStats{}
	}
	stats := tileCacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Puts: c.puts.Load()}
	for i := range c.shards {
		shard := &c.shards[i]
		shard.mu.Lock()
		stats.UsedBytes += shard.used
		stats.MaxBytes += shard.maxBytes
		shard.mu.Unlock()
	}
	c.largeTile.mu.Lock()
	stats.UsedBytes += c.largeTile.used
	stats.MaxBytes += c.largeTile.maxBytes
	c.largeTile.mu.Unlock()
	return stats
}

type tileCacheShard struct {
	mu       sync.Mutex
	maxBytes int64
	used     int64
	entries  map[tiles.Key]*list.Element
	lru      list.List
	// front is an immutable snapshot of the most recently used payload. A
	// pan/zoom workload commonly asks for the same tile many times (including
	// by concurrent clients), so this lets that dominant hit path bypass the
	// LRU lock entirely. It is cleared before its payload is evicted, keeping
	// the cache's byte bound intact.
	front atomic.Pointer[tileCacheValue]
}

type tileCacheEntry struct {
	key      tiles.Key
	value    *tileCacheValue
	checksum string
	size     int64
}

// tileCacheValue is never mutated after publication. Readers can therefore
// safely use a snapshot returned by shard.front without holding the shard
// mutex, while eviction swaps the entry to a checksum-only value.
type tileCacheValue struct {
	key      tiles.Key
	data     []byte
	checksum string
}

func newTileCache(maxBytes int64) *tileCache {
	if maxBytes <= 0 {
		return nil
	}
	cache := &tileCache{}
	// Reserve one shared lane for large but valid raster/vector payloads. A
	// strictly even shard split would make a 32 MiB cache reject every tile
	// larger than 2 MiB even when almost all of its budget is unused.
	largeBudget := maxBytes / tileCacheLargeFraction
	normalBudget := maxBytes - largeBudget
	perShard := normalBudget / tileCacheShardCount
	remainder := normalBudget % tileCacheShardCount
	for index := range cache.shards {
		cache.shards[index].maxBytes = perShard
		if int64(index) < remainder {
			cache.shards[index].maxBytes++
		}
		cache.shards[index].entries = make(map[tiles.Key]*list.Element)
	}
	cache.largeTile.maxBytes = largeBudget
	cache.largeTile.entries = make(map[tiles.Key]*list.Element)
	return cache
}

func (c *tileCache) get(key tiles.Key) ([]byte, string, bool) {
	if c == nil {
		return nil, "", false
	}
	normal := c.shard(key)
	if data, checksum, found := c.getFast(normal, key); found {
		c.hits.Add(1)
		return data, checksum, true
	}
	// Large payloads live in their shared lane. Probe its lock-free front
	// before taking the normal lane's mutex on a miss so a popular aerial tile
	// stays cheap too.
	if data, checksum, found := c.getFast(&c.largeTile, key); found {
		c.hits.Add(1)
		return data, checksum, true
	}
	if data, checksum, found := c.getFrom(normal, key); found {
		c.hits.Add(1)
		return data, checksum, true
	}
	if data, checksum, found := c.getFrom(&c.largeTile, key); found {
		c.hits.Add(1)
		return data, checksum, true
	}
	c.misses.Add(1)
	return nil, "", false
}

func (c *tileCache) put(key tiles.Key, data []byte, checksum string) {
	if c == nil {
		return
	}
	c.puts.Add(1)
	size := tileCacheChecksumSize(checksum) + int64(len(data))
	shard := c.shard(key)
	if size > shard.maxBytes {
		shard = &c.largeTile
	}
	if size > shard.maxBytes {
		// Large payloads still benefit from retaining their small immutable
		// checksum: a later uncached read avoids hashing the complete tile.
		c.putChecksum(c.shard(key), key, checksum)
		return
	}
	c.putInto(shard, key, data, checksum, size)
}

func (c *tileCache) getFrom(shard *tileCacheShard, key tiles.Key) ([]byte, string, bool) {
	if data, checksum, found := c.getFast(shard, key); found {
		return data, checksum, true
	}
	shard.mu.Lock()
	element, found := shard.entries[key]
	if !found {
		shard.mu.Unlock()
		return nil, "", false
	}
	entry := element.Value.(*tileCacheEntry)
	if entry.value == nil {
		shard.mu.Unlock()
		return nil, "", false
	}
	shard.lru.MoveToFront(element)
	value := entry.value
	shard.front.Store(value)
	shard.mu.Unlock()
	return value.data, value.checksum, true
}

func (c *tileCache) getFast(shard *tileCacheShard, key tiles.Key) ([]byte, string, bool) {
	value := shard.front.Load()
	if value == nil || value.key != key {
		return nil, "", false
	}
	return value.data, value.checksum, true
}

// checksum returns a retained digest even when the payload has been evicted.
// Artifacts are immutable for the lifetime of Server, so the digest remains
// valid across any number of pager reads of the same key.
func (c *tileCache) checksum(key tiles.Key) (string, bool) {
	if c == nil {
		return "", false
	}
	if checksum, found := c.checksumFrom(c.shard(key), key); found {
		return checksum, true
	}
	return c.checksumFrom(&c.largeTile, key)
}

func (c *tileCache) checksumFrom(shard *tileCacheShard, key tiles.Key) (string, bool) {
	shard.mu.Lock()
	element, found := shard.entries[key]
	if !found {
		shard.mu.Unlock()
		return "", false
	}
	checksum := element.Value.(*tileCacheEntry).checksum
	shard.mu.Unlock()
	return checksum, true
}

func (c *tileCache) putInto(shard *tileCacheShard, key tiles.Key, data []byte, checksum string, size int64) {
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if element, found := shard.entries[key]; found {
		entry := element.Value.(*tileCacheEntry)
		shard.used -= entry.size
		shard.front.CompareAndSwap(entry.value, nil)
		entry.value = &tileCacheValue{key: key, data: data, checksum: checksum}
		entry.checksum, entry.size = checksum, size
		shard.used += size
		shard.lru.MoveToFront(element)
	} else {
		element := shard.lru.PushFront(&tileCacheEntry{
			key:      key,
			value:    &tileCacheValue{key: key, data: data, checksum: checksum},
			checksum: checksum,
			size:     size,
		})
		shard.entries[key] = element
		shard.used += size
	}
	// Insertion/update changes the list front. Publishing that new immutable
	// payload keeps later hits lock-free only while it is still the true LRU
	// front; other list mutations similarly replace or clear this pointer.
	shard.front.Store(shard.entries[key].Value.(*tileCacheEntry).value)
	for shard.used > shard.maxBytes {
		element := shard.lru.Back()
		if element == nil {
			break
		}
		entry := element.Value.(*tileCacheEntry)
		if entry.value != nil {
			shard.front.CompareAndSwap(entry.value, nil)
			entry.value = nil
			shard.used -= entry.size - tileCacheChecksumSize(entry.checksum)
			entry.size = tileCacheChecksumSize(entry.checksum)
			continue
		}
		delete(shard.entries, entry.key)
		shard.lru.Remove(element)
		shard.used -= entry.size
	}
}

func (c *tileCache) putChecksum(shard *tileCacheShard, key tiles.Key, checksum string) {
	size := tileCacheChecksumSize(checksum)
	if size > shard.maxBytes {
		return
	}
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if element, found := shard.entries[key]; found {
		entry := element.Value.(*tileCacheEntry)
		if entry.value != nil {
			return
		}
		shard.used -= entry.size
		entry.checksum = checksum
		entry.size = size
		shard.used += size
		shard.lru.MoveToFront(element)
	} else {
		element := shard.lru.PushFront(&tileCacheEntry{key: key, checksum: checksum, size: size})
		shard.entries[key] = element
		shard.used += size
	}
	// A checksum-only update becomes the LRU front, so a previously published
	// payload can no longer skip the lock without weakening LRU recency.
	shard.front.Store(nil)
	for shard.used > shard.maxBytes {
		element := shard.lru.Back()
		if element == nil {
			break
		}
		entry := element.Value.(*tileCacheEntry)
		if entry.value != nil {
			shard.front.CompareAndSwap(entry.value, nil)
			entry.value = nil
			shard.used -= entry.size - tileCacheChecksumSize(entry.checksum)
			entry.size = tileCacheChecksumSize(entry.checksum)
			continue
		}
		delete(shard.entries, entry.key)
		shard.lru.Remove(element)
		shard.used -= entry.size
	}
}

func tileCacheChecksumSize(checksum string) int64 {
	return int64(len(checksum)) + tileCacheEntryOverhead
}

func (c *tileCache) shard(key tiles.Key) *tileCacheShard {
	// Key components are already bounded integers. A small integer mix keeps
	// adjacent x/y coordinates distributed across independent LRU locks.
	mixed := uint64(uint32(key.Z))*0x9e3779b1 ^ uint64(uint32(key.X))*0x85ebca6b ^ uint64(uint32(key.Y))*0xc2b2ae35
	return &c.shards[mixed%tileCacheShardCount]
}
