package offline

import (
	"context"
	"errors"
	"sync"
)

// MemoryStore is a race-safe reference store used by tests, demos and short
// lived command-line clients. It copies tile payloads at the public boundary.
type MemoryStore struct {
	mu        sync.RWMutex
	manifests map[string]Manifest
	tiles     map[memoryTileKey]memoryStoredTile
}

// memoryTileKey is comparable, so it can be used directly as a map key. This
// avoids allocating and concatenating a string for every in-memory cache
// lookup, which is the common hot path during an already-warm sync.
type memoryTileKey struct {
	dataset  string
	revision string
	key      TileKey
}

// memoryStoredTile keeps the verification state private to the store. A tile
// admitted by Synchronizer has already had its checksum checked and is copied
// before it is retained, so a later warm sync does not need to hash the same
// immutable payload again. Public PutTile entries remain unverified when they
// carry a checksum: callers may supply an arbitrary checksum and the
// Synchronizer must continue to detect a mismatch before reusing them.
type memoryStoredTile struct {
	tile     Tile
	verified bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{manifests: make(map[string]Manifest), tiles: make(map[memoryTileKey]memoryStoredTile)}
}

func (s *MemoryStore) GetManifest(ctx context.Context, dataset string) (Manifest, bool, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, false, err
	}
	if err := validateIdentifier("dataset", dataset, 256); err != nil {
		return Manifest{}, false, err
	}
	s.mu.RLock()
	manifest, found := s.manifests[dataset]
	s.mu.RUnlock()
	return manifest, found, nil
}

func (s *MemoryStore) PutManifest(ctx context.Context, manifest Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.manifests[manifest.Dataset] = manifest
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) GetTile(ctx context.Context, dataset, revision string, key TileKey) (Tile, bool, error) {
	if err := ctx.Err(); err != nil {
		return Tile{}, false, err
	}
	if err := validateCacheKey(dataset, revision, key); err != nil {
		return Tile{}, false, err
	}
	s.mu.RLock()
	stored, found := s.tiles[memoryTileKey{dataset: dataset, revision: revision, key: key}]
	s.mu.RUnlock()
	if !found {
		return Tile{}, false, nil
	}
	return stored.tile.Clone(), true, nil
}

func (s *MemoryStore) PutTile(ctx context.Context, dataset, revision string, key TileKey, tile Tile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCacheKey(dataset, revision, key); err != nil {
		return err
	}
	if err := tile.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.tiles[memoryTileKey{dataset: dataset, revision: revision, key: key}] = memoryStoredTile{
		tile:     tile.Clone(),
		verified: tile.Checksum == "",
	}
	s.mu.Unlock()
	return nil
}

// getVerifiedTile is the Synchronizer-only fast path for an exact
// *MemoryStore. It retains the store-owned immutable payload rather than
// cloning it on every warm-cache reuse. Entries inserted through public
// PutTile still have their checksum verified once here; only a tile copied
// after Synchronizer verification can take the zero-hash path.
func (s *MemoryStore) getVerifiedTile(ctx context.Context, dataset, revision string, key TileKey) (Tile, bool, error) {
	if err := ctx.Err(); err != nil {
		return Tile{}, false, err
	}
	s.mu.RLock()
	stored, found := s.tiles[memoryTileKey{dataset: dataset, revision: revision, key: key}]
	s.mu.RUnlock()
	if !found {
		return Tile{}, false, nil
	}
	if !stored.verified {
		if err := verifyTile(stored.tile); err != nil {
			return Tile{}, false, err
		}
	}
	return stored.tile, true, nil
}

// putVerifiedTile retains an independent, already validated copy. The public
// boundary remains defensive for callers that do not have that guarantee.
func (s *MemoryStore) putVerifiedTile(ctx context.Context, dataset, revision string, key TileKey, tile Tile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.tiles[memoryTileKey{dataset: dataset, revision: revision, key: key}] = memoryStoredTile{tile: tile.Clone(), verified: true}
	s.mu.Unlock()
	return nil
}

// hasVerifiedTiles reports whether every requested key is present in this
// store as an immutable, Synchronizer-verified entry. It deliberately does
// not treat public checksum-bearing PutTile entries as trusted: those still
// take the regular path, which verifies them before reuse. The caller has
// already validated the request, manifest and cache namespace.
func (s *MemoryStore) hasVerifiedTiles(ctx context.Context, dataset, revision string, request SyncRequest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	err := request.visit(ctx, func(key TileKey) error {
		s.mu.RLock()
		stored, found := s.tiles[memoryTileKey{dataset: dataset, revision: revision, key: key}]
		s.mu.RUnlock()
		if !found || !stored.verified {
			return errMemoryStoreCacheMiss
		}
		return nil
	})
	if errors.Is(err, errMemoryStoreCacheMiss) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *MemoryStore) DeleteRevision(ctx context.Context, dataset, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateIdentifier("dataset", dataset, 256); err != nil {
		return err
	}
	if err := validateIdentifier("revision", revision, 512); err != nil {
		return err
	}
	s.mu.Lock()
	for key := range s.tiles {
		if key.dataset == dataset && key.revision == revision {
			delete(s.tiles, key)
		}
	}
	s.mu.Unlock()
	return nil
}

func validateCacheKey(dataset, revision string, key TileKey) error {
	if err := validateIdentifier("dataset", dataset, 256); err != nil {
		return err
	}
	if err := validateIdentifier("revision", revision, 512); err != nil {
		return err
	}
	return key.Validate()
}
