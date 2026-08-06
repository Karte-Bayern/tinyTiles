package offline

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	defaultSyncConcurrency = 4
	maxSyncConcurrency     = 32
)

var errMemoryStoreCacheMiss = errors.New("memory store cache miss")

// SyncRequest selects a bounded set of tiles to retain offline. Ranges are
// generated directly into workers; the implementation never expands an entire
// region into a tile slice. Ranges and explicit keys must not overlap.
type SyncRequest struct {
	Dataset       string             `json:"dataset,omitempty"`
	Ranges        []TileRange        `json:"ranges,omitempty"`
	Keys          []TileKey          `json:"keys,omitempty"`
	Concurrency   int                `json:"concurrency,omitempty"`
	PrunePrevious bool               `json:"prune_previous,omitempty"`
	Progress      func(SyncProgress) `json:"-"`
}

// SyncProgress is emitted after a manifest was fetched and after each tile is
// retained or reused. Callbacks are invoked from workers and must be safe for
// concurrent use.
type SyncProgress struct {
	Phase      string  `json:"phase"`
	Total      uint64  `json:"total"`
	Completed  uint64  `json:"completed"`
	Downloaded uint64  `json:"downloaded"`
	Reused     uint64  `json:"reused"`
	Key        TileKey `json:"key,omitempty"`
}

// SyncResult describes a completed cache switch. A prune error is reported but
// does not roll back the new manifest: retaining an old immutable namespace is
// safe, whereas rolling back a fully synchronized new revision is not useful.
type SyncResult struct {
	Dataset        string `json:"dataset"`
	Revision       string `json:"revision"`
	Total          uint64 `json:"total"`
	Downloaded     uint64 `json:"downloaded"`
	Reused         uint64 `json:"reused"`
	ManifestWasNew bool   `json:"manifest_was_new"`
	PruneError     string `json:"prune_error,omitempty"`
}

// Synchronizer commits a new active manifest only after every requested tile
// for that revision is present. If a fetch fails, a previous manifest remains
// usable and the next run can safely resume from retained revisioned tiles.
type Synchronizer struct {
	Store   Store
	Fetcher Fetcher
	mu      sync.Mutex
}

func (s *Synchronizer) Sync(ctx context.Context, request SyncRequest) (SyncResult, error) {
	if s == nil {
		return SyncResult{}, errors.New("offline synchronizer is nil")
	}
	// One synchronizer serializes manifest publication. A caller that shares a
	// Store across components should likewise share this synchronizer instance.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Store == nil {
		return SyncResult{}, errors.New("offline sync store is nil")
	}
	if s.Fetcher == nil {
		return SyncResult{}, errors.New("offline sync fetcher is nil")
	}
	httpFetcher, fetcherResponsesVerified := s.Fetcher.(*HTTPFetcher)
	if err := request.validate(); err != nil {
		return SyncResult{}, err
	}
	manifest, err := s.Fetcher.FetchManifest(ctx)
	if err != nil {
		return SyncResult{}, fmt.Errorf("fetch sync manifest: %w", err)
	}
	// HTTPFetcher validates the decoded manifest before it returns. Keeping the
	// generic Fetcher boundary defensive avoids an extra URL parse/allocation
	// per warm HTTP sync without widening the trusted surface.
	if !fetcherResponsesVerified {
		if err := manifest.Validate(); err != nil {
			return SyncResult{}, fmt.Errorf("validate sync manifest: %w", err)
		}
	}
	if request.Dataset != "" && request.Dataset != manifest.Dataset {
		return SyncResult{}, fmt.Errorf("requested dataset %q does not match manifest dataset %q", request.Dataset, manifest.Dataset)
	}
	verifiedStore := newSyncStoreFastPath(s.Store, manifest.Dataset, manifest.Revision)
	total, err := request.total()
	if err != nil {
		return SyncResult{}, err
	}
	emitProgress(request.Progress, SyncProgress{Phase: "manifest", Total: total})
	previous, hadPrevious, err := s.Store.GetManifest(ctx, manifest.Dataset)
	if err != nil {
		return SyncResult{}, fmt.Errorf("read cached manifest: %w", err)
	}
	// A completed MemoryStore sync has immutable, independently copied tiles
	// and an active manifest for this revision. With no progress callback to
	// preserve per-worker callback ordering, bypass the worker pool entirely
	// when the whole request is already warm. This avoids channels, goroutines,
	// cancellation machinery and repeated checksum hashing on the dominant
	// offline-open path. A partial or externally seeded cache falls through to
	// the normal, fully validating pipeline.
	if request.Progress == nil && hadPrevious && previous.Revision == manifest.Revision {
		if memoryStore, ok := s.Store.(*MemoryStore); ok {
			allCached, err := memoryStore.hasVerifiedTiles(ctx, manifest.Dataset, manifest.Revision, request)
			if err != nil {
				return SyncResult{}, fmt.Errorf("read cached tiles: %w", err)
			}
			if allCached {
				return s.publish(ctx, request, manifest, previous, hadPrevious, total, 0, total)
			}
		}
	}

	workers := request.workerCount()
	jobs := make(chan TileKey, workers*2)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var state syncState
	state.total = total
	var workersWG sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	fail := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			for key := range jobs {
				if err := workCtx.Err(); err != nil {
					return
				}
				var cached Tile
				var found bool
				var err error
				if verifiedStore != nil {
					cached, found, err = verifiedStore.get(workCtx, manifest.Dataset, manifest.Revision, key)
				} else {
					cached, found, err = s.Store.GetTile(workCtx, manifest.Dataset, manifest.Revision, key)
				}
				if err != nil {
					fail(fmt.Errorf("read cached tile %s: %w", key, err))
					return
				}
				if found {
					if verifiedStore == nil {
						if err := verifyTile(cached); err != nil {
							fail(fmt.Errorf("validate cached tile %s: %w", key, err))
							return
						}
					}
					state.advance(false, key, request.Progress)
					continue
				}
				var tile Tile
				if fetcherResponsesVerified {
					tile, err = httpFetcher.fetchVerifiedTile(workCtx, manifest, key)
				} else {
					tile, err = s.Fetcher.FetchTile(workCtx, manifest, key)
				}
				if err != nil {
					fail(fmt.Errorf("fetch tile %s: %w", key, err))
					return
				}
				if !fetcherResponsesVerified {
					if err := verifyTile(tile); err != nil {
						fail(fmt.Errorf("verify tile %s: %w", key, err))
						return
					}
				}
				var storeErr error
				if verifiedStore != nil {
					storeErr = verifiedStore.put(workCtx, manifest.Dataset, manifest.Revision, key, tile)
				} else {
					storeErr = s.Store.PutTile(workCtx, manifest.Dataset, manifest.Revision, key, tile)
				}
				if storeErr != nil {
					fail(fmt.Errorf("store tile %s: %w", key, storeErr))
					return
				}
				state.advance(true, key, request.Progress)
			}
		}()
	}
	produceErr := request.visit(workCtx, func(key TileKey) error {
		select {
		case jobs <- key:
			return nil
		case <-workCtx.Done():
			return workCtx.Err()
		}
	})
	close(jobs)
	workersWG.Wait()
	errMu.Lock()
	workerErr := firstErr
	errMu.Unlock()
	if workerErr != nil {
		return SyncResult{}, workerErr
	}
	if produceErr != nil {
		return SyncResult{}, produceErr
	}
	if err := ctx.Err(); err != nil {
		return SyncResult{}, err
	}
	return s.publish(ctx, request, manifest, previous, hadPrevious, total, state.downloadedCount(), state.reusedCount())
}

func (s *Synchronizer) publish(ctx context.Context, request SyncRequest, manifest, previous Manifest, hadPrevious bool, total, downloaded, reused uint64) (SyncResult, error) {
	// The existing active manifest already durably names this exact immutable
	// revision. Rewriting it after a warm sync turns every offline-open into an
	// unnecessary atomic write plus fsync on FileStore. Persist it only when a
	// field actually changed; new tile records have already been atomically
	// published by their individual store operations.
	if !hadPrevious || previous != manifest || !canReuseActiveManifest(s.Store) {
		if err := s.Store.PutManifest(ctx, manifest); err != nil {
			return SyncResult{}, fmt.Errorf("publish cached manifest: %w", err)
		}
	}
	result := SyncResult{
		Dataset:        manifest.Dataset,
		Revision:       manifest.Revision,
		Total:          total,
		Downloaded:     downloaded,
		Reused:         reused,
		ManifestWasNew: !hadPrevious || previous.Revision != manifest.Revision,
	}
	if request.PrunePrevious && hadPrevious && previous.Revision != manifest.Revision {
		if err := s.Store.DeleteRevision(ctx, previous.Dataset, previous.Revision); err != nil {
			result.PruneError = err.Error()
		}
	}
	emitProgress(request.Progress, SyncProgress{Phase: "published", Total: total, Completed: total, Downloaded: result.Downloaded, Reused: result.Reused})
	return result, nil
}

func (r SyncRequest) validate() error {
	if r.Dataset != "" {
		if err := validateIdentifier("dataset", r.Dataset, 256); err != nil {
			return err
		}
	}
	if r.Concurrency < 0 || r.Concurrency > maxSyncConcurrency {
		return fmt.Errorf("sync concurrency must be between 0 and %d", maxSyncConcurrency)
	}
	for i, tileRange := range r.Ranges {
		if err := tileRange.Validate(); err != nil {
			return fmt.Errorf("range %d: %w", i, err)
		}
		for previous := 0; previous < i; previous++ {
			if rangesOverlap(tileRange, r.Ranges[previous]) {
				return fmt.Errorf("ranges %d and %d overlap", previous, i)
			}
		}
	}
	seenKeys := make(map[TileKey]struct{}, len(r.Keys))
	for _, key := range r.Keys {
		if err := key.Validate(); err != nil {
			return err
		}
		if _, found := seenKeys[key]; found {
			return fmt.Errorf("duplicate explicit tile key %s", key)
		}
		seenKeys[key] = struct{}{}
		for _, tileRange := range r.Ranges {
			if key.Z == tileRange.Z && key.X >= tileRange.XMin && key.X <= tileRange.XMax && key.Y >= tileRange.YMin && key.Y <= tileRange.YMax {
				return fmt.Errorf("explicit tile key %s overlaps a requested range", key)
			}
		}
	}
	return nil
}

func (r SyncRequest) total() (uint64, error) {
	total := uint64(len(r.Keys))
	for _, tileRange := range r.Ranges {
		count, err := tileRange.Count()
		if err != nil {
			return 0, err
		}
		if total > ^uint64(0)-count {
			return 0, errors.New("sync tile count overflows uint64")
		}
		total += count
	}
	return total, nil
}

func (r SyncRequest) workerCount() int {
	if r.Concurrency > 0 {
		return r.Concurrency
	}
	return defaultSyncConcurrency
}

func (r SyncRequest) visit(ctx context.Context, fn func(TileKey) error) error {
	for _, key := range r.Keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(key); err != nil {
			return err
		}
	}
	for _, tileRange := range r.Ranges {
		if err := tileRange.Visit(ctx, fn); err != nil {
			return err
		}
	}
	return nil
}

func rangesOverlap(a, b TileRange) bool {
	return a.Z == b.Z && a.XMin <= b.XMax && b.XMin <= a.XMax && a.YMin <= b.YMax && b.YMin <= a.YMax
}

func verifyTile(tile Tile) error {
	if tile.Checksum == "" {
		return nil
	}
	if len(tile.Checksum) != sha256.Size*2 {
		return fmt.Errorf("invalid tile SHA-256 length %d", len(tile.Checksum))
	}
	digest := sha256.Sum256(tile.Data)
	if !equalDigestHex(digest, tile.Checksum) {
		return errors.New("SHA-256 checksum mismatch")
	}
	return nil
}

// equalDigestHex compares a SHA-256 digest with its (case-insensitive) hex
// representation without allocating a second encoded digest for every tile.
func equalDigestHex(digest [sha256.Size]byte, encoded string) bool {
	for index, value := range digest {
		if hexNibble(encoded[index*2]) != value>>4 || hexNibble(encoded[index*2+1]) != value&0x0f {
			return false
		}
	}
	return true
}

func hexNibble(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10
	default:
		return 0xff
	}
}

type syncState struct {
	total      uint64
	completed  atomic.Uint64
	downloaded atomic.Uint64
	reused     atomic.Uint64
	progressMu sync.Mutex
}

func (s *syncState) advance(downloaded bool, key TileKey, progress func(SyncProgress)) {
	// Most syncs do not request progress reporting. Avoid contending on a
	// shared mutex in that throughput-oriented path. When a callback is
	// present, serialize snapshots so every callback receives internally
	// consistent completed/downloaded/reused totals.
	if progress != nil {
		s.progressMu.Lock()
	}
	if downloaded {
		s.downloaded.Add(1)
	} else {
		s.reused.Add(1)
	}
	completed := s.completed.Add(1)
	if progress != nil {
		next := SyncProgress{
			Phase:      "tile",
			Total:      s.total,
			Completed:  completed,
			Downloaded: s.downloaded.Load(),
			Reused:     s.reused.Load(),
			Key:        key,
		}
		s.progressMu.Unlock()
		emitProgress(progress, next)
	}
}

func (s *syncState) downloadedCount() uint64 {
	return s.downloaded.Load()
}

func (s *syncState) reusedCount() uint64 {
	return s.reused.Load()
}

func emitProgress(fn func(SyncProgress), progress SyncProgress) {
	if fn != nil {
		fn(progress)
	}
}
