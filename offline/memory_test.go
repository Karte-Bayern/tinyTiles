package offline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMemoryStoreCopiesTilePayloadsAndDeletesRevision(t *testing.T) {
	store := NewMemoryStore()
	manifest := testManifest("r1")
	key := TileKey{Z: 2, X: 1, Y: 2}
	payload := []byte("source")
	tile := checkedTile(payload)
	if err := store.PutTile(context.Background(), manifest.Dataset, manifest.Revision, key, tile); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'S'
	tile.Data[1] = 'X'
	got, found, err := store.GetTile(context.Background(), manifest.Dataset, manifest.Revision, key)
	if err != nil || !found || string(got.Data) != "source" {
		t.Fatalf("stored tile=%q found=%t err=%v", got.Data, found, err)
	}
	got.Data[0] = 'X'
	got, found, err = store.GetTile(context.Background(), manifest.Dataset, manifest.Revision, key)
	if err != nil || !found || string(got.Data) != "source" {
		t.Fatalf("retrieved tile leaked mutation=%q found=%t err=%v", got.Data, found, err)
	}
	if err := store.PutManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRevision(context.Background(), manifest.Dataset, manifest.Revision); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetTile(context.Background(), manifest.Dataset, manifest.Revision, key); err != nil || found {
		t.Fatalf("deleted tile found=%t err=%v", found, err)
	}
}

func TestMemoryStoreConcurrentAccess(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	const workers = 8
	const perWorker = 32
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < perWorker; i++ {
				key := TileKey{Z: 6, X: worker, Y: i}
				tile := checkedTile([]byte{byte(worker), byte(i)})
				if err := store.PutTile(ctx, "concurrent", "r1", key, tile); err != nil {
					t.Errorf("put %s: %v", key, err)
					return
				}
				got, found, err := store.GetTile(ctx, "concurrent", "r1", key)
				if err != nil || !found || string(got.Data) != string(tile.Data) {
					t.Errorf("get %s found=%t data=%x err=%v", key, found, got.Data, err)
					return
				}
			}
		}()
	}
	group.Wait()
}

type wrappedMemoryStore struct{ *MemoryStore }

func TestSyncFastPathUsesOnlyExactMemoryStore(t *testing.T) {
	store := NewMemoryStore()
	if newSyncStoreFastPath(store, "demo", "r1") == nil {
		t.Fatal("MemoryStore did not select its verified fast path")
	}
	if newSyncStoreFastPath(&wrappedMemoryStore{MemoryStore: store}, "demo", "r1") != nil {
		t.Fatal("MemoryStore wrapper bypassed its Store implementation")
	}
}

func TestMemoryStoreVerifiedEntriesArePrivateAndWarm(t *testing.T) {
	store := NewMemoryStore()
	manifest := testManifest("verified")
	key := TileKey{Z: 1, X: 0, Y: 0}
	tile := checkedTile([]byte("verified payload"))
	if err := store.putVerifiedTile(context.Background(), manifest.Dataset, manifest.Revision, key, tile); err != nil {
		t.Fatal(err)
	}
	tile.Data[0] = 'V'
	got, found, err := store.getVerifiedTile(context.Background(), manifest.Dataset, manifest.Revision, key)
	if err != nil || !found || string(got.Data) != "verified payload" {
		t.Fatalf("verified tile=%q found=%t err=%v", got.Data, found, err)
	}
	all, err := store.hasVerifiedTiles(context.Background(), manifest.Dataset, manifest.Revision, SyncRequest{Keys: []TileKey{key}})
	if err != nil || !all {
		t.Fatalf("verified warm cache all=%t err=%v", all, err)
	}
}

func TestMemoryStoreWarmScanDoesNotTrustPublicChecksummedTiles(t *testing.T) {
	store := NewMemoryStore()
	manifest := testManifest("untrusted")
	key := TileKey{Z: 1, X: 0, Y: 0}
	if err := store.PutTile(context.Background(), manifest.Dataset, manifest.Revision, key, checkedTile([]byte("public payload"))); err != nil {
		t.Fatal(err)
	}
	all, err := store.hasVerifiedTiles(context.Background(), manifest.Dataset, manifest.Revision, SyncRequest{Keys: []TileKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	if all {
		t.Fatal("public checksummed tile bypassed synchronization verification")
	}
}

func TestMemoryStoreWarmScanHonorsCancellation(t *testing.T) {
	store := NewMemoryStore()
	manifest := testManifest("canceled")
	keys := []TileKey{{Z: 2, X: 0, Y: 0}, {Z: 2, X: 1, Y: 0}}
	for _, key := range keys {
		if err := store.putVerifiedTile(context.Background(), manifest.Dataset, manifest.Revision, key, checkedTile([]byte(key.String()))); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.hasVerifiedTiles(&cancelAfterContext{remaining: 2}, manifest.Dataset, manifest.Revision, SyncRequest{Keys: keys})
	if all || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled warm scan all=%t err=%v", all, err)
	}
}

// cancelAfterContext is deterministic test scaffolding for a cancellation
// between tiles, without relying on a timer racing a short in-memory scan.
type cancelAfterContext struct{ remaining int }

func (c *cancelAfterContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterContext) Done() <-chan struct{}       { return nil }
func (c *cancelAfterContext) Err() error {
	if c.remaining == 0 {
		return context.Canceled
	}
	c.remaining--
	return nil
}
func (c *cancelAfterContext) Value(any) any { return nil }
