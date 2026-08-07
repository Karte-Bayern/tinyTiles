//go:build sqliteimport

package server

import (
	"bytes"
	"errors"
	"testing"
	"time"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestRouteTileKeysFollowRouteOrderAndBudget(t *testing.T) {
	route := []RoutePoint{{Latitude: 0, Longitude: -135}, {Latitude: 0, Longitude: 45}}
	keys, truncated, err := routeTileKeys(route, 2, 0, 8)
	if err != nil || truncated {
		t.Fatalf("routeTileKeys err=%v truncated=%t", err, truncated)
	}
	want := []tiles.Key{{Z: 2, X: 0, Y: 1}, {Z: 2, X: 1, Y: 1}, {Z: 2, X: 2, Y: 1}}
	if !equalKeys(keys, want) {
		t.Fatalf("keys=%v, want %v", keys, want)
	}
	keys, truncated, err = routeTileKeys(route, 2, 0, 2)
	if err != nil || !truncated || !equalKeys(keys, want[:2]) {
		t.Fatalf("bounded keys=%v truncated=%t err=%v", keys, truncated, err)
	}
}

func TestRouteTileKeysCrossesAntimeridianViaShortPath(t *testing.T) {
	route := []RoutePoint{{Latitude: 0, Longitude: 170}, {Latitude: 0, Longitude: -170}}
	keys, truncated, err := routeTileKeys(route, 3, 0, 8)
	if err != nil || truncated {
		t.Fatalf("routeTileKeys err=%v truncated=%t", err, truncated)
	}
	want := []tiles.Key{{Z: 3, X: 7, Y: 3}, {Z: 3, X: 0, Y: 3}}
	if !equalKeys(keys, want) {
		t.Fatalf("keys=%v, want %v", keys, want)
	}
}

func TestRouteTileKeysWarmRadiusAndRejectInvalidCoordinates(t *testing.T) {
	keys, truncated, err := routeTileKeys([]RoutePoint{{Latitude: 0, Longitude: 0}}, 2, 1, 16)
	if err != nil || truncated || len(keys) != 9 {
		t.Fatalf("radius keys=%v truncated=%t err=%v", keys, truncated, err)
	}
	if _, _, err := routeTileKeys([]RoutePoint{{Latitude: 91, Longitude: 0}}, 2, 0, 1); err == nil {
		t.Fatal("invalid latitude accepted")
	}
}

func TestServerPrefetchRouteWarmsKnownTile(t *testing.T) {
	server := testServer(t)
	t.Cleanup(server.Close)
	point := RoutePoint{Latitude: 40, Longitude: -45}
	result, err := server.PrefetchRoute(t.Context(), []RoutePoint{point}, RoutePrefetchOptions{Zoom: 2})
	if err != nil || result.Queued != 1 || result.Truncated {
		t.Fatalf("prefetch result=%#v err=%v", result, err)
	}
	key := tiles.Key{Z: 2, X: 1, Y: 2}
	deadline := time.Now().Add(time.Second)
	for {
		data, _, found := server.gen.Load().tileCache.get(key)
		if found {
			if !bytes.Equal(data, []byte{1, 2, 3}) {
				t.Fatalf("prefetched data=%x", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prefetched tile did not reach cache")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerPrefetchRouteRequiresUsableTileCache(t *testing.T) {
	dataset := testDataset(t)
	server, err := New(Config{Dataset: dataset, DatasetID: "fixture", TileCacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	_, err = server.PrefetchRoute(t.Context(), []RoutePoint{{Latitude: 0, Longitude: 0}}, RoutePrefetchOptions{Zoom: 2})
	if !errors.Is(err, ErrPredictiveCachingDisabled) {
		t.Fatalf("prefetch error=%v, want ErrPredictiveCachingDisabled", err)
	}
}

func TestServerPrefetchKeysSkipsWarmAndDuplicateTiles(t *testing.T) {
	server := testServer(t)
	t.Cleanup(server.Close)
	key := tiles.Key{Z: 2, X: 1, Y: 2}
	server.gen.Load().tileCache.put(key, []byte{1, 2, 3}, "fixture-checksum")
	result, err := server.PrefetchKeys(t.Context(), []tiles.Key{key, key})
	if err != nil {
		t.Fatal(err)
	}
	if result.Considered != 2 || result.Queued != 0 || result.AlreadyCached != 1 || result.AlreadyQueued != 1 || result.Dropped != 0 || result.Truncated {
		t.Fatalf("prefetch keys result=%#v", result)
	}
	if _, err := server.PrefetchKeys(t.Context(), []tiles.Key{{Z: 2, X: 4, Y: 0}}); err == nil {
		t.Fatal("invalid key accepted")
	}
	server.prefetchMaxTiles = 1
	result, err = server.PrefetchKeys(t.Context(), []tiles.Key{key, tiles.Key{Z: 2, X: 0, Y: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Considered != 1 || result.AlreadyCached != 1 || !result.Truncated {
		t.Fatalf("bounded prefetch keys result=%#v", result)
	}
}

func TestServerPrefetchRangeWarmsViewportAndBoundsWork(t *testing.T) {
	server := testServer(t)
	t.Cleanup(server.Close)
	server.prefetchMaxTiles = 1
	key := tiles.Key{Z: 2, X: 1, Y: 2}
	result, err := server.PrefetchRange(t.Context(), tiles.Range{Z: 2, XMin: 1, XMax: 2, YMin: 2, YMax: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Considered != 1 || result.Queued != 1 || !result.Truncated {
		t.Fatalf("prefetch range result=%#v", result)
	}
	deadline := time.Now().Add(time.Second)
	for {
		data, _, found := server.gen.Load().tileCache.get(key)
		if found {
			if !bytes.Equal(data, []byte{1, 2, 3}) {
				t.Fatalf("prefetched data=%x", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("viewport tile did not reach cache")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerPrefetchXYZRangeFlipsRowsOnce(t *testing.T) {
	server := testServer(t)
	t.Cleanup(server.Close)
	// Fixture TMS 2/1/2 is XYZ 2/1/1.
	key := tiles.Key{Z: 2, X: 1, Y: 2}
	result, err := server.PrefetchXYZRange(t.Context(), XYZRange{Z: 2, XMin: 1, XMax: 1, YMin: 1, YMax: 1})
	if err != nil || result.Queued != 1 || result.Truncated {
		t.Fatalf("prefetch XYZ range result=%#v err=%v", result, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		data, _, found := server.gen.Load().tileCache.get(key)
		if found {
			if !bytes.Equal(data, []byte{1, 2, 3}) {
				t.Fatalf("prefetched XYZ data=%x", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("XYZ viewport tile did not reach cache")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := server.PrefetchXYZRange(t.Context(), XYZRange{Z: 2, XMin: 1, XMax: 1, YMin: 4, YMax: 4}); err == nil {
		t.Fatal("invalid XYZ range accepted")
	}
}

func TestTilePrefetcherCoalescesPendingKeys(t *testing.T) {
	prefetcher := newTilePrefetcher(nil, 0, 1)
	defer prefetcher.close()
	first := tiles.Key{Z: 2, X: 1, Y: 2}
	second := tiles.Key{Z: 2, X: 2, Y: 2}
	if got := prefetcher.enqueue(first); got != prefetchEnqueued {
		t.Fatalf("first enqueue=%v, want queued", got)
	}
	if got := prefetcher.enqueue(first); got != prefetchAlreadyQueued {
		t.Fatalf("duplicate enqueue=%v, want already queued", got)
	}
	if got := prefetcher.enqueue(second); got != prefetchDropped {
		t.Fatalf("full queue enqueue=%v, want dropped", got)
	}
}

func equalKeys(left, right []tiles.Key) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
