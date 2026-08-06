package server

import (
	"context"
	"errors"
	"sync"

	"github.com/Karte-Bayern/tinyTiles/offline"
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
)

var (
	// ErrPredictiveCachingDisabled means predictive work cannot be retained,
	// either because the tile cache was disabled or Server.Close was called.
	ErrPredictiveCachingDisabled = errors.New("tinytiles server: predictive caching is disabled")
)

// RoutePoint is one WGS84 coordinate in the order produced by routing APIs.
// It is the same type a client uses to build offline.RouteSyncRequest, so a
// trusted application can compute one route corridor and both prefetch it on
// the server and synchronize it to a mobile client with the same points.
type RoutePoint = offline.RoutePoint

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
	if s == nil || s.gen.Load().tileCache == nil {
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

// routeTileKeys rasterizes route into tinySQL TMS keys via the shared,
// dependency-free implementation in the offline package, which a client also
// uses to build a RouteSyncRequest for the same corridor.
func routeTileKeys(route []RoutePoint, zoom, radius, maxTiles int) ([]tiles.Key, bool, error) {
	offlineKeys, truncated, err := offline.RouteTileKeys(route, zoom, radius, maxTiles)
	if err != nil {
		return nil, false, err
	}
	keys := make([]tiles.Key, len(offlineKeys))
	for index, key := range offlineKeys {
		keys[index] = tiles.Key{Z: key.Z, X: key.X, Y: key.Y}
	}
	return keys, truncated, nil
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
					gen := prefetcher.server.gen.Load()
					_, _, _, _ = prefetcher.server.lookupTile(context.Background(), gen, key)
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
