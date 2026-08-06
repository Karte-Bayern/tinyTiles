package server

import (
	"context"
	"errors"
	"math"
	"sync"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

const (
	// DefaultPrefetchWorkers bounds concurrent predictive reads. They use the
	// same Dataset reader pool as HTTP requests, so a small default preserves
	// foreground capacity.
	DefaultPrefetchWorkers = 2
	// DefaultPrefetchQueue is the bounded number of predicted tiles waiting for
	// a background read.
	DefaultPrefetchQueue = 512
	// DefaultPrefetchMaxTiles is the per-route work budget. Routes are emitted
	// from their start, which prioritizes the part a navigator reaches next.
	DefaultPrefetchMaxTiles = 1024
	maxRoutePrefetchRadius  = 8
	webMercatorLatitude     = 85.0511287798066
)

var (
	// ErrPredictiveCachingDisabled means predictive work cannot be retained,
	// either because the tile cache was disabled or Server.Close was called.
	ErrPredictiveCachingDisabled = errors.New("tinytiles server: predictive caching is disabled")
)

// RoutePoint is one WGS84 coordinate in the order produced by routing APIs.
type RoutePoint struct {
	Latitude  float64
	Longitude float64
}

// RoutePrefetchOptions controls the bounded prediction around a route.
// Zoom is required. Radius warms neighboring tiles around the route center
// line; zero warms only crossed tiles. MaxTiles zero uses the server default.
type RoutePrefetchOptions struct {
	Zoom     int
	Radius   int
	MaxTiles int
}

// PrefetchResult describes work accepted by the bounded background queue.
// Queued jobs may include already cached keys; workers discard those cheaply.
type PrefetchResult struct {
	Considered int
	Queued     int
	Dropped    int
	Truncated  bool
}

// PrefetchRoute predicts tiles along a WGS84 route and submits them to a
// bounded low-priority queue. It deliberately is an application API rather
// than an unauthenticated HTTP route: callers normally invoke it after a
// trusted routing result has been computed. Server.Close stops the workers.
func (s *Server) PrefetchRoute(ctx context.Context, route []RoutePoint, options RoutePrefetchOptions) (PrefetchResult, error) {
	if s == nil || s.tileCache == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	if err := ctx.Err(); err != nil {
		return PrefetchResult{}, err
	}
	if options.Zoom < 0 || options.Zoom > 30 {
		return PrefetchResult{}, errors.New("tinytiles server: route prefetch zoom must be between 0 and 30")
	}
	if options.Radius < 0 || options.Radius > maxRoutePrefetchRadius {
		return PrefetchResult{}, errors.New("tinytiles server: route prefetch radius must be between 0 and 8")
	}
	if options.MaxTiles < 0 {
		return PrefetchResult{}, errors.New("tinytiles server: route prefetch max tiles must not be negative")
	}
	if len(route) == 0 {
		return PrefetchResult{}, errors.New("tinytiles server: route prefetch requires at least one point")
	}
	maxTiles := options.MaxTiles
	if maxTiles == 0 {
		maxTiles = s.prefetchMaxTiles
	}
	prefetcher := s.routePrefetcher()
	if prefetcher == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	keys, truncated, err := routeTileKeys(route, options.Zoom, options.Radius, maxTiles)
	if err != nil {
		return PrefetchResult{}, err
	}
	result := PrefetchResult{Considered: len(keys), Truncated: truncated}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if prefetcher.enqueue(key) {
			result.Queued++
		} else {
			result.Dropped++
		}
	}
	return result, nil
}

func routeTileKeys(route []RoutePoint, zoom, radius, maxTiles int) ([]tiles.Key, bool, error) {
	if maxTiles < 1 {
		return nil, true, nil
	}
	points := make([]xyzPoint, len(route))
	for index, point := range route {
		xyz, err := routePointXYZ(point, zoom)
		if err != nil {
			return nil, false, err
		}
		points[index] = xyz
	}
	n := 1 << zoom
	seen := make(map[tiles.Key]struct{}, min(maxTiles, 256))
	keys := make([]tiles.Key, 0, min(maxTiles, 256))
	truncated := false
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
				key := tiles.Key{Z: zoom, X: xx, Y: n - 1 - yy}
				if _, exists := seen[key]; exists {
					continue
				}
				if len(keys) == maxTiles {
					truncated = true
					return false
				}
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
		}
		return true
	}
	if len(points) == 1 {
		emit(points[0].x, points[0].y)
		return keys, truncated, nil
	}
	for index := 1; index < len(points); index++ {
		start, end := points[index-1], points[index]
		// Follow the short world-wrapping segment at the antimeridian instead
		// of warming almost every tile around the globe.
		if abs(end.x-start.x) > n/2 {
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
	return keys, truncated, nil
}

type xyzPoint struct{ x, y int }

func routePointXYZ(point RoutePoint, zoom int) (xyzPoint, error) {
	if math.IsNaN(point.Latitude) || math.IsInf(point.Latitude, 0) || point.Latitude < -90 || point.Latitude > 90 || math.IsNaN(point.Longitude) || math.IsInf(point.Longitude, 0) || point.Longitude < -180 || point.Longitude > 180 {
		return xyzPoint{}, errors.New("tinytiles server: route coordinate is outside WGS84 bounds")
	}
	latitude := math.Max(-webMercatorLatitude, math.Min(webMercatorLatitude, point.Latitude))
	n := float64(uint64(1) << zoom)
	x := int(math.Floor((point.Longitude + 180) / 360 * n))
	latRadians := latitude * math.Pi / 180
	y := int(math.Floor((1 - math.Log(math.Tan(latRadians)+1/math.Cos(latRadians))/math.Pi) / 2 * n))
	limit := int(n) - 1
	return xyzPoint{x: min(max(x, 0), limit), y: min(max(y, 0), limit)}, nil
}

// rasterizeRouteSegment visits every grid cell crossed by a segment, in route
// order. Bresenham's integer form is deterministic and avoids accumulating
// floating-point error on long routes.
func rasterizeRouteSegment(start, end xyzPoint, visit func(x, y int) bool) bool {
	x, y := start.x, start.y
	dx, dy := abs(end.x-start.x), -abs(end.y-start.y)
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

type tilePrefetcher struct {
	server *Server
	jobs   chan tiles.Key
	stop   chan struct{}
	done   sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

func newTilePrefetcher(server *Server, workers, queue int) *tilePrefetcher {
	prefetcher := &tilePrefetcher{server: server, jobs: make(chan tiles.Key, queue), stop: make(chan struct{})}
	prefetcher.done.Add(workers)
	for range workers {
		go func() {
			defer prefetcher.done.Done()
			for {
				// Prefer shutdown over processing buffered work.
				select {
				case <-prefetcher.stop:
					return
				default:
				}
				select {
				case <-prefetcher.stop:
					return
				case key := <-prefetcher.jobs:
					_, _, _, _ = prefetcher.server.lookupTile(context.Background(), key)
				}
			}
		}()
	}
	return prefetcher
}

func (p *tilePrefetcher) enqueue(key tiles.Key) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.jobs <- key:
		return true
	default:
		return false
	}
}

func (p *tilePrefetcher) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	p.mu.Unlock()
	p.done.Wait()
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
