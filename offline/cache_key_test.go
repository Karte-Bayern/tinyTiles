package offline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
)

func rawCacheComponent(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestCacheComponentMatchesDirectHash(t *testing.T) {
	for _, value := range []string{"", "dach", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "dataset with spaces"} {
		if got, want := cacheComponent(value), rawCacheComponent(value); got != want {
			t.Fatalf("cacheComponent(%q) = %q, want %q", value, got, want)
		}
		// A second call must return the identical memoized value.
		if got, want := cacheComponent(value), rawCacheComponent(value); got != want {
			t.Fatalf("memoized cacheComponent(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestPersistentKeysUnchanged(t *testing.T) {
	key := TileKey{Z: 8, X: 137, Y: 167}
	if got, want := persistentTileKey("dach", "rev-1", key), "tile:"+rawCacheComponent("dach")+":"+rawCacheComponent("rev-1")+":"+key.String(); got != want {
		t.Fatalf("persistentTileKey = %q, want %q", got, want)
	}
	if got, want := persistentManifestKey("dach"), "manifest:"+rawCacheComponent("dach"); got != want {
		t.Fatalf("persistentManifestKey = %q, want %q", got, want)
	}
}

func TestCacheComponentConcurrentAccessIsRaceSafe(t *testing.T) {
	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				value := fmt.Sprintf("dataset-%d", i%5)
				if got, want := cacheComponent(value), rawCacheComponent(value); got != want {
					t.Errorf("cacheComponent(%q) = %q, want %q", value, got, want)
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestCacheComponentCacheStaysBounded(t *testing.T) {
	for i := 0; i < cacheComponentCacheLimit*3; i++ {
		value := fmt.Sprintf("bound-probe-%d", i)
		if got, want := cacheComponent(value), rawCacheComponent(value); got != want {
			t.Fatalf("cacheComponent(%q) = %q, want %q", value, got, want)
		}
	}
	cacheComponentMu.RLock()
	size := len(cacheComponentMemo)
	cacheComponentMu.RUnlock()
	if size > cacheComponentCacheLimit {
		t.Fatalf("cacheComponentMemo grew to %d entries, want <= %d", size, cacheComponentCacheLimit)
	}
}
