//go:build js && wasm

package offline

import "context"

// syncStoreFastPath is selected only for the exact built-in Store type. A
// wrapper that overrides Store behavior intentionally remains on the generic,
// fully defensive path.
type syncStoreFastPath interface {
	get(context.Context, string, string, TileKey) (Tile, bool, error)
	put(context.Context, string, string, TileKey, Tile) error
}

type indexedDBStoreSyncFastPath struct{ store *IndexedDBStore }

type memoryStoreSyncFastPath struct{ store *MemoryStore }

func newSyncStoreFastPath(store Store, _, _ string) syncStoreFastPath {
	if indexedDBStore, ok := store.(*IndexedDBStore); ok {
		return indexedDBStoreSyncFastPath{store: indexedDBStore}
	}
	if memoryStore, ok := store.(*MemoryStore); ok {
		return memoryStoreSyncFastPath{store: memoryStore}
	}
	return nil
}

// See the native implementation: only built-in stores have no observable
// PutManifest side effects beyond retaining the current manifest.
func canReuseActiveManifest(store Store) bool {
	switch store.(type) {
	case *IndexedDBStore, *MemoryStore:
		return true
	default:
		return false
	}
}

func (p indexedDBStoreSyncFastPath) get(ctx context.Context, dataset, revision string, key TileKey) (Tile, bool, error) {
	return p.store.GetTile(ctx, dataset, revision, key)
}

func (p indexedDBStoreSyncFastPath) put(ctx context.Context, dataset, revision string, key TileKey, tile Tile) error {
	return p.store.putVerifiedTile(ctx, dataset, revision, key, tile)
}

func (p memoryStoreSyncFastPath) get(ctx context.Context, dataset, revision string, key TileKey) (Tile, bool, error) {
	return p.store.getVerifiedTile(ctx, dataset, revision, key)
}

func (p memoryStoreSyncFastPath) put(ctx context.Context, dataset, revision string, key TileKey, tile Tile) error {
	return p.store.putVerifiedTile(ctx, dataset, revision, key, tile)
}
