package offline

import (
	"errors"
	"fmt"
	"math"
)

const (
	// maxRouteZoom bounds Zoom the same way TileKey bounds an individual
	// coordinate's zoom.
	maxRouteZoom = 30
	// maxRouteRadius keeps a neighboring-tile expansion a small, predictable
	// multiple of the crossed-tile count instead of an accidental region sync.
	maxRouteRadius = 8
	// webMercatorLatitude is the maximum latitude the standard Web Mercator
	// tile grid represents; PrefetchRoute and RouteTileKeys both clamp to it.
	webMercatorLatitude = 85.0511287798066
)

// RoutePoint is one WGS84 coordinate in the order a routing API produced it.
type RoutePoint struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// RouteTileKeys rasterizes a WGS84 route into the ordered, deduplicated set of
// TMS tiles it crosses at zoom, optionally widened by radius neighboring
// tiles around each crossed cell. It is the client-safe building block behind
// RouteSyncRequest and the server's route-based predictive prefetch: both
// need the same corridor, not a bounding rectangle around the route's
// extremes, which typically contains far more tiles than the route actually
// crosses — the gap grows with route length, and is largest for a long
// diagonal or winding route where the bounding box's empty corners dominate.
//
// maxTiles bounds the returned set; once reached, truncated is true and
// rasterization stops without silently returning an unbounded slice. Results
// are ordered by route traversal, so a truncated result still prioritizes the
// tiles nearest the route's start.
func RouteTileKeys(route []RoutePoint, zoom, radius, maxTiles int) (keys []TileKey, truncated bool, err error) {
	if zoom < 0 || zoom > maxRouteZoom {
		return nil, false, fmt.Errorf("route zoom must be between 0 and %d", maxRouteZoom)
	}
	if radius < 0 || radius > maxRouteRadius {
		return nil, false, fmt.Errorf("route radius must be between 0 and %d", maxRouteRadius)
	}
	if maxTiles < 0 {
		return nil, false, errors.New("route max tiles must not be negative")
	}
	if len(route) == 0 {
		return nil, false, errors.New("route requires at least one point")
	}
	if maxTiles == 0 {
		return nil, true, nil
	}
	points := make([]routeXY, len(route))
	for index, point := range route {
		xy, err := routePointXY(point, zoom)
		if err != nil {
			return nil, false, err
		}
		points[index] = xy
	}
	n := 1 << zoom
	seen := make(map[TileKey]struct{}, min(maxTiles, 256))
	result := make([]TileKey, 0, min(maxTiles, 256))
	emit := func(x, y int) bool {
		for offsetY := -radius; offsetY <= radius; offsetY++ {
			yy := y + offsetY
			if yy < 0 || yy >= n {
				continue
			}
			for offsetX := -radius; offsetX <= radius; offsetX++ {
				xx := x + offsetX
				if xx < 0 || xx >= n {
					continue
				}
				// The route projection above is XYZ-convention (north at
				// row 0); TileKey is TMS, so this is the one required flip.
				key := TileKey{Z: zoom, X: xx, Y: n - 1 - yy}
				if _, exists := seen[key]; exists {
					continue
				}
				if len(result) == maxTiles {
					truncated = true
					return false
				}
				seen[key] = struct{}{}
				result = append(result, key)
			}
		}
		return true
	}
	if len(points) == 1 {
		emit(points[0].x, points[0].y)
		return result, truncated, nil
	}
	for index := 1; index < len(points); index++ {
		start, end := points[index-1], points[index]
		// Follow the short world-wrapping segment at the antimeridian instead
		// of warming almost every tile around the globe.
		if absInt(end.x-start.x) > n/2 {
			if end.x > start.x {
				end.x -= n
			} else {
				end.x += n
			}
		}
		if !rasterizeRouteSegment(start, end, func(x, y int) bool {
			return emit((x%n+n)%n, y)
		}) {
			break
		}
	}
	return result, truncated, nil
}

type routeXY struct{ x, y int }

func routePointXY(point RoutePoint, zoom int) (routeXY, error) {
	if math.IsNaN(point.Latitude) || math.IsInf(point.Latitude, 0) || point.Latitude < -90 || point.Latitude > 90 || math.IsNaN(point.Longitude) || math.IsInf(point.Longitude, 0) || point.Longitude < -180 || point.Longitude > 180 {
		return routeXY{}, errors.New("route coordinate is outside WGS84 bounds")
	}
	latitude := math.Max(-webMercatorLatitude, math.Min(webMercatorLatitude, point.Latitude))
	n := float64(uint64(1) << zoom)
	x := int(math.Floor((point.Longitude + 180) / 360 * n))
	latRadians := latitude * math.Pi / 180
	y := int(math.Floor((1 - math.Log(math.Tan(latRadians)+1/math.Cos(latRadians))/math.Pi) / 2 * n))
	limit := int(n) - 1
	return routeXY{x: min(max(x, 0), limit), y: min(max(y, 0), limit)}, nil
}

// rasterizeRouteSegment visits every grid cell crossed by a segment, in route
// order, using Bresenham's integer form so long routes accumulate no
// floating-point error.
func rasterizeRouteSegment(start, end routeXY, visit func(x, y int) bool) bool {
	x, y := start.x, start.y
	dx, dy := absInt(end.x-start.x), -absInt(end.y-start.y)
	sx, sy := -1, -1
	if x < end.x {
		sx = 1
	}
	if y < end.y {
		sy = 1
	}
	err := dx + dy
	for {
		if !visit(x, y) {
			return false
		}
		if x == end.x && y == end.y {
			return true
		}
		twice := 2 * err
		if twice >= dy {
			err += dy
			x += sx
		}
		if twice <= dx {
			err += dx
			y += sy
		}
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// RouteSyncOptions controls RouteSyncRequest. Zoom is required; the rest have
// the same meaning and defaults as SyncRequest and RouteTileKeys.
type RouteSyncOptions struct {
	// Zoom is the tile level to synchronize.
	Zoom int `json:"zoom"`
	// Radius widens the corridor by this many neighboring tiles around each
	// crossed cell. Zero retains only the exactly-crossed tiles.
	Radius int `json:"radius,omitempty"`
	// MaxTiles bounds the request. Zero selects DefaultRouteSyncMaxTiles.
	MaxTiles int `json:"max_tiles,omitempty"`
	// Concurrency, PrunePrevious and Progress are passed through to the
	// resulting SyncRequest unchanged.
	Concurrency   int                `json:"concurrency,omitempty"`
	PrunePrevious bool               `json:"prune_previous,omitempty"`
	Progress      func(SyncProgress) `json:"-"`
}

// DefaultRouteSyncMaxTiles bounds a RouteSyncRequest when the caller leaves
// MaxTiles at zero. It matches the server's DefaultPrefetchMaxTiles so a
// client's route sync and a trusted server's route prefetch agree on a
// sensible per-route budget without either side needing to know the other's
// default.
const DefaultRouteSyncMaxTiles = 1024

// RouteSyncRequest builds a SyncRequest that retains only the tiles along
// route instead of the bounding rectangle spanning its extremes. For a route
// with any meaningful length or diagonal extent, the corridor is
// substantially smaller: a long diagonal drive's bounding box can contain
// several times as many tiles as the road actually crosses, and every one of
// those extra tiles is a tile a mobile client would otherwise download,
// store and never render. Call Sync with the result exactly as with any
// other SyncRequest; Keys already carries the deduplicated, budget-bounded
// corridor.
//
// truncated reports whether MaxTiles cut the corridor short; a caller that
// wants to guarantee full route coverage should treat that as a signal to
// raise MaxTiles or split the route into shorter successive sync calls
// (each one immediately usable: a partial sync still commits tiles it
// retained).
func RouteSyncRequest(route []RoutePoint, options RouteSyncOptions) (request SyncRequest, truncated bool, err error) {
	maxTiles := options.MaxTiles
	if maxTiles == 0 {
		maxTiles = DefaultRouteSyncMaxTiles
	}
	keys, truncated, err := RouteTileKeys(route, options.Zoom, options.Radius, maxTiles)
	if err != nil {
		return SyncRequest{}, false, err
	}
	return SyncRequest{
		Keys:          keys,
		Concurrency:   options.Concurrency,
		PrunePrevious: options.PrunePrevious,
		Progress:      options.Progress,
	}, truncated, nil
}
