package offline

import (
	"context"
	"testing"
)

func TestRouteTileKeysFollowRouteOrderAndBudget(t *testing.T) {
	route := []RoutePoint{{Latitude: 0, Longitude: -135}, {Latitude: 0, Longitude: 45}}
	keys, truncated, err := RouteTileKeys(route, 2, 0, 8)
	if err != nil || truncated {
		t.Fatalf("RouteTileKeys err=%v truncated=%t", err, truncated)
	}
	want := []TileKey{{Z: 2, X: 0, Y: 1}, {Z: 2, X: 1, Y: 1}, {Z: 2, X: 2, Y: 1}}
	if !equalTileKeys(keys, want) {
		t.Fatalf("keys=%v, want %v", keys, want)
	}
	keys, truncated, err = RouteTileKeys(route, 2, 0, 2)
	if err != nil || !truncated || !equalTileKeys(keys, want[:2]) {
		t.Fatalf("bounded keys=%v truncated=%t err=%v", keys, truncated, err)
	}
}

func TestRouteTileKeysCrossesAntimeridianViaShortPath(t *testing.T) {
	route := []RoutePoint{{Latitude: 0, Longitude: 170}, {Latitude: 0, Longitude: -170}}
	keys, truncated, err := RouteTileKeys(route, 3, 0, 8)
	if err != nil || truncated {
		t.Fatalf("RouteTileKeys err=%v truncated=%t", err, truncated)
	}
	want := []TileKey{{Z: 3, X: 7, Y: 3}, {Z: 3, X: 0, Y: 3}}
	if !equalTileKeys(keys, want) {
		t.Fatalf("keys=%v, want %v", keys, want)
	}
}

func TestRouteTileKeysWarmRadiusAndRejectInvalidCoordinates(t *testing.T) {
	keys, truncated, err := RouteTileKeys([]RoutePoint{{Latitude: 0, Longitude: 0}}, 2, 1, 16)
	if err != nil || truncated || len(keys) != 9 {
		t.Fatalf("radius keys=%v truncated=%t err=%v", keys, truncated, err)
	}
	if _, _, err := RouteTileKeys([]RoutePoint{{Latitude: 91, Longitude: 0}}, 2, 0, 1); err == nil {
		t.Fatal("invalid latitude accepted")
	}
	if _, _, err := RouteTileKeys(nil, 2, 0, 1); err == nil {
		t.Fatal("empty route accepted")
	}
	if _, _, err := RouteTileKeys([]RoutePoint{{Latitude: 0, Longitude: 0}}, -1, 0, 1); err == nil {
		t.Fatal("negative zoom accepted")
	}
	if _, _, err := RouteTileKeys([]RoutePoint{{Latitude: 0, Longitude: 0}}, 2, 9, 1); err == nil {
		t.Fatal("radius above the maximum accepted")
	}
}

// TestRouteTileKeysCorridorIsSmallerThanBoundingRange is the concrete claim
// behind RouteSyncRequest: a long, diagonal route's minimal TMS bounding
// rectangle contains substantially more tiles than the corridor the route
// actually crosses. A mobile client that synced the bounding TileRange
// instead of the corridor would download, store and never render every tile
// in that gap.
func TestRouteTileKeysCorridorIsSmallerThanBoundingRange(t *testing.T) {
	const zoom = 10
	route := []RoutePoint{{Latitude: 52.5, Longitude: 13.4}, {Latitude: 48.1, Longitude: 11.6}} // Berlin -> Munich
	keys, truncated, err := RouteTileKeys(route, zoom, 1, 4096)
	if err != nil || truncated {
		t.Fatalf("RouteTileKeys err=%v truncated=%t", err, truncated)
	}
	if len(keys) == 0 {
		t.Fatal("corridor is empty")
	}
	minX, maxX, minY, maxY := keys[0].X, keys[0].X, keys[0].Y, keys[0].Y
	for _, key := range keys[1:] {
		minX, maxX = min(minX, key.X), max(maxX, key.X)
		minY, maxY = min(minY, key.Y), max(maxY, key.Y)
	}
	boundingRangeTiles := (maxX - minX + 1) * (maxY - minY + 1)
	if len(keys) >= boundingRangeTiles {
		t.Fatalf("corridor tiles=%d is not smaller than its bounding range=%d", len(keys), boundingRangeTiles)
	}
	t.Logf("corridor=%d tiles, bounding range=%d tiles (%.1f%% of the range)", len(keys), boundingRangeTiles, 100*float64(len(keys))/float64(boundingRangeTiles))
}

func TestRouteSyncRequestBuildsValidRequestAndSyncsOnlyTheCorridor(t *testing.T) {
	route := []RoutePoint{{Latitude: 52.5, Longitude: 13.4}, {Latitude: 48.1, Longitude: 11.6}}
	request, truncated, err := RouteSyncRequest(route, RouteSyncOptions{Zoom: 10, Radius: 1, Concurrency: 4})
	if err != nil || truncated {
		t.Fatalf("RouteSyncRequest err=%v truncated=%t", err, truncated)
	}
	if len(request.Ranges) != 0 {
		t.Fatalf("RouteSyncRequest used %d ranges, want the corridor expressed only as Keys", len(request.Ranges))
	}
	if err := request.validate(); err != nil {
		t.Fatalf("built request is invalid: %v", err)
	}

	store := NewMemoryStore()
	fetcher := &fakeFetcher{manifest: testManifest("route-revision")}
	synchronizer := &Synchronizer{Store: store, Fetcher: fetcher}
	result, err := synchronizer.Sync(context.Background(), request)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Downloaded != uint64(len(request.Keys)) || result.Total != uint64(len(request.Keys)) {
		t.Fatalf("result=%#v, want downloaded/total=%d", result, len(request.Keys))
	}
	for _, key := range request.Keys {
		if _, found, err := store.GetTile(context.Background(), "demo", "route-revision", key); err != nil || !found {
			t.Fatalf("corridor tile %s missing after sync: found=%t err=%v", key, found, err)
		}
	}
}

func TestRouteSyncRequestDefaultsMaxTilesAndRejectsInvalidZoom(t *testing.T) {
	request, truncated, err := RouteSyncRequest([]RoutePoint{{Latitude: 0, Longitude: 0}}, RouteSyncOptions{Zoom: 4})
	if err != nil || truncated || len(request.Keys) == 0 {
		t.Fatalf("request=%#v truncated=%t err=%v", request, truncated, err)
	}
	if _, _, err := RouteSyncRequest([]RoutePoint{{Latitude: 0, Longitude: 0}}, RouteSyncOptions{Zoom: -1}); err == nil {
		t.Fatal("negative zoom accepted")
	}
}

func equalTileKeys(left, right []TileKey) bool {
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
