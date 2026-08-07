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

// XYZRange is an inclusive slippy-map viewport. It intentionally mirrors
// tiles.Range but uses the top-left-origin row convention that map clients
// already use; PrefetchXYZRange performs the one required conversion to the
// artifact's TMS keys.
type XYZRange struct {
	Z    int
	XMin int
	XMax int
	YMin int
	YMax int
}

func (r XYZRange) validate() error {
	// TMS and XYZ use the same numeric coordinate domain; only their row
	// origin differs, so tiles.Range's bound validation applies directly.
	return (tiles.Range{Z: r.Z, XMin: r.XMin, XMax: r.XMax, YMin: r.YMin, YMax: r.YMax}).Validate()
}

// PrefetchResult describes work submitted to predictive caching. Considered is
// the number of caller-supplied keys within the configured work budget;
// AlreadyCached and AlreadyQueued explain why a considered key was not added
// to the background queue. Dropped means the bounded queue had no capacity.
type PrefetchResult struct {
	Considered    int
	Queued        int
	AlreadyCached int
	AlreadyQueued int
	Dropped       int
	Truncated     bool
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
	keys, truncated, err := routeTileKeys(route, options.Zoom, options.Radius, maxTiles)
	if err != nil {
		return PrefetchResult{}, err
	}
	return s.prefetchTiles(ctx, keys, truncated)
}

// PrefetchKeys queues an ordered TMS key list supplied by a trusted
// application. It is useful for a map viewport, a search result cluster, or a
// client prediction that has already determined the exact tiles it will need.
// At most Config.PrefetchMaxTiles keys are considered, and duplicate or
// already-warm payloads do not consume queue capacity. This is deliberately
// not exposed as an unauthenticated HTTP endpoint: a caller decides which
// prediction is trustworthy enough to spend cache memory on.
func (s *Server) PrefetchKeys(ctx context.Context, keys []tiles.Key) (PrefetchResult, error) {
	if s == nil || s.gen.Load().tileCache == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	if err := ctx.Err(); err != nil {
		return PrefetchResult{}, err
	}
	limit := s.prefetchMaxTiles
	truncated := len(keys) > limit
	if truncated {
		keys = keys[:limit]
	}
	return s.prefetchTiles(ctx, keys, truncated)
}

// PrefetchRange queues the TMS tiles in a rectangular viewport. Like
// PrefetchKeys, it is a trusted application API with the server's bounded
// prefetch budget; it avoids allocating a tile list larger than that budget.
// The iteration order is z/x/y, which gives callers predictable truncation
// when a rectangle exceeds Config.PrefetchMaxTiles.
func (s *Server) PrefetchRange(ctx context.Context, tileRange tiles.Range) (PrefetchResult, error) {
	if s == nil || s.gen.Load().tileCache == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	if err := ctx.Err(); err != nil {
		return PrefetchResult{}, err
	}
	if err := tileRange.Validate(); err != nil {
		return PrefetchResult{}, err
	}
	limit := s.prefetchMaxTiles
	keys := make([]tiles.Key, 0, min(limit, 256))
	for x := tileRange.XMin; x <= tileRange.XMax; x++ {
		for y := tileRange.YMin; y <= tileRange.YMax; y++ {
			if err := ctx.Err(); err != nil {
				return PrefetchResult{Considered: len(keys)}, err
			}
			if len(keys) == limit {
				return s.prefetchTiles(ctx, keys, true)
			}
			keys = append(keys, tiles.Key{Z: tileRange.Z, X: x, Y: y})
		}
	}
	return s.prefetchTiles(ctx, keys, false)
}

// PrefetchXYZRange queues a browser-facing XYZ viewport. It preserves XYZ
// north-to-south traversal order when work is truncated, while storing the
// corresponding TMS keys in the predictive cache. Applications serving an
// ordinary web map should prefer this over manually flipping y coordinates.
func (s *Server) PrefetchXYZRange(ctx context.Context, tileRange XYZRange) (PrefetchResult, error) {
	if s == nil || s.gen.Load().tileCache == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	if err := ctx.Err(); err != nil {
		return PrefetchResult{}, err
	}
	if err := tileRange.validate(); err != nil {
		return PrefetchResult{}, err
	}
	limit := s.prefetchMaxTiles
	keys := make([]tiles.Key, 0, min(limit, 256))
	maxY := (1 << tileRange.Z) - 1
	for x := tileRange.XMin; x <= tileRange.XMax; x++ {
		for y := tileRange.YMin; y <= tileRange.YMax; y++ {
			if err := ctx.Err(); err != nil {
				return PrefetchResult{Considered: len(keys)}, err
			}
			if len(keys) == limit {
				return s.prefetchTiles(ctx, keys, true)
			}
			keys = append(keys, tiles.Key{Z: tileRange.Z, X: x, Y: maxY - y})
		}
	}
	return s.prefetchTiles(ctx, keys, false)
}

func (s *Server) prefetchTiles(ctx context.Context, keys []tiles.Key, truncated bool) (PrefetchResult, error) {
	if s == nil || s.gen.Load().tileCache == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	prefetcher := s.routePrefetcher()
	if prefetcher == nil {
		return PrefetchResult{}, ErrPredictiveCachingDisabled
	}
	gen := s.gen.Load()
	result := PrefetchResult{Truncated: truncated}
	seen := make(map[tiles.Key]struct{}, min(len(keys), 256))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := key.Validate(); err != nil {
			return result, err
		}
		result.Considered++
		if _, duplicate := seen[key]; duplicate {
			result.AlreadyQueued++
			continue
		}
		seen[key] = struct{}{}
		if _, _, cached := gen.tileCache.get(key); cached {
			result.AlreadyCached++
			continue
		}
		switch prefetcher.enqueue(key) {
		case prefetchEnqueued:
			result.Queued++
		case prefetchAlreadyQueued:
			result.AlreadyQueued++
		default:
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

	mu      sync.Mutex
	closed  bool
	pending map[tiles.Key]struct{}
}

type prefetchEnqueueResult uint8

const (
	prefetchDropped prefetchEnqueueResult = iota
	prefetchEnqueued
	prefetchAlreadyQueued
)

func newTilePrefetcher(server *Server, workers, queue int) *tilePrefetcher {
	prefetcher := &tilePrefetcher{server: server, jobs: make(chan tiles.Key, queue), stop: make(chan struct{}), pending: make(map[tiles.Key]struct{}, queue)}
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
					prefetcher.complete(key)
				}
			}
		}()
	}
	return prefetcher
}

func (p *tilePrefetcher) enqueue(key tiles.Key) prefetchEnqueueResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return prefetchDropped
	}
	if _, pending := p.pending[key]; pending {
		return prefetchAlreadyQueued
	}
	select {
	case p.jobs <- key:
		p.pending[key] = struct{}{}
		return prefetchEnqueued
	default:
		return prefetchDropped
	}
}

func (p *tilePrefetcher) complete(key tiles.Key) {
	p.mu.Lock()
	delete(p.pending, key)
	p.mu.Unlock()
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
