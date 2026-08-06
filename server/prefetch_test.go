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
	xyz, err := routePointXYZ(point, 2)
	if err != nil || xyz != (xyzPoint{x: 1, y: 1}) {
		t.Fatalf("route point xyz=%#v err=%v", xyz, err)
	}
	result, err := server.PrefetchRoute(t.Context(), []RoutePoint{point}, RoutePrefetchOptions{Zoom: 2})
	if err != nil || result.Queued != 1 || result.Truncated {
		t.Fatalf("prefetch result=%#v err=%v", result, err)
	}
	key := tiles.Key{Z: 2, X: 1, Y: 2}
	deadline := time.Now().Add(time.Second)
	for {
		data, _, found := server.tileCache.get(key)
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
