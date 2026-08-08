package offline

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// cacheComponentCacheLimit bounds the memoized digest cache. A real caller
// reuses a small, session-stable set of dataset and revision strings across
// every tile it reads or writes; this only guards against a caller driving
// an unbounded number of distinct values, in which case the cache is reset
// instead of growing without limit.
const cacheComponentCacheLimit = 256

var (
	cacheComponentMu   sync.RWMutex
	cacheComponentMemo = map[string]string{}
)

// cacheComponent hashes a dataset or revision string into its path/key-safe
// component. GetTile and PutTile call it on every request with a value drawn
// from that tiny, session-stable set, so memoizing it turns the repeated
// SHA-256 computation and hex encode on the per-tile hot path into a map
// lookup after the first call for a given value. The returned component for
// any given input never changes.
func cacheComponent(value string) string {
	cacheComponentMu.RLock()
	encoded, ok := cacheComponentMemo[value]
	cacheComponentMu.RUnlock()
	if ok {
		return encoded
	}
	digest := sha256.Sum256([]byte(value))
	encoded = hex.EncodeToString(digest[:])
	cacheComponentMu.Lock()
	if len(cacheComponentMemo) >= cacheComponentCacheLimit {
		cacheComponentMemo = map[string]string{}
	}
	cacheComponentMemo[value] = encoded
	cacheComponentMu.Unlock()
	return encoded
}

func persistentTileKey(dataset, revision string, key TileKey) string {
	return "tile:" + cacheComponent(dataset) + ":" + cacheComponent(revision) + ":" + key.String()
}

func persistentManifestKey(dataset string) string {
	return "manifest:" + cacheComponent(dataset)
}
