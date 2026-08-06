//go:build !js || !wasm

package offline

import (
	"context"
	"path/filepath"
	"strconv"
)

// syncStoreFastPath is selected only for the exact built-in Store type. A
// wrapper that overrides Store behavior intentionally remains on the generic,
// fully defensive path.
type syncStoreFastPath interface {
	get(context.Context, string, string, TileKey) (Tile, bool, error)
	put(context.Context, string, string, TileKey, Tile) error
}

type fileStoreSyncFastPath struct {
	store     *FileStore
	directory string
}

type memoryStoreSyncFastPath struct{ store *MemoryStore }

func newSyncStoreFastPath(store Store, dataset, revision string) syncStoreFastPath {
	if fileStore, ok := store.(*FileStore); ok {
		return fileStoreSyncFastPath{
			store:     fileStore,
			directory: filepath.Join(fileStore.root, "tiles", cacheComponent(dataset), cacheComponent(revision)),
		}
	}
	if memoryStore, ok := store.(*MemoryStore); ok {
		return memoryStoreSyncFastPath{store: memoryStore}
	}
	return nil
}

// canReuseActiveManifest is deliberately limited to exact built-in stores.
// A custom Store may use PutManifest as an externally visible commit hook even
// if the value is unchanged, so Synchronizer preserves its historic call
// semantics for wrappers and third-party implementations.
func canReuseActiveManifest(store Store) bool {
	switch store.(type) {
	case *FileStore, *MemoryStore:
		return true
	default:
		return false
	}
}

func (p fileStoreSyncFastPath) get(ctx context.Context, dataset, revision string, key TileKey) (Tile, bool, error) {
	return p.store.getTileAt(ctx, p.tilePath(key))
}

func (p fileStoreSyncFastPath) put(ctx context.Context, dataset, revision string, key TileKey, tile Tile) error {
	return p.store.putVerifiedTileAt(ctx, p.tilePath(key), tile)
}

func (p fileStoreSyncFastPath) tilePath(key TileKey) string {
	return filepath.Join(p.directory, strconv.Itoa(key.Z), strconv.Itoa(key.X), strconv.Itoa(key.Y)+".tile")
}

func (p memoryStoreSyncFastPath) get(ctx context.Context, dataset, revision string, key TileKey) (Tile, bool, error) {
	return p.store.getVerifiedTile(ctx, dataset, revision, key)
}

func (p memoryStoreSyncFastPath) put(ctx context.Context, dataset, revision string, key TileKey, tile Tile) error {
	return p.store.putVerifiedTile(ctx, dataset, revision, key, tile)
}
