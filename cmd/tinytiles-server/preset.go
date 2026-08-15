package main

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/server"
)

// serverPreset bundles Config field values tuned for one of tinyTiles'
// documented serving use cases, so an operator can pass -preset instead of
// hand-tuning -readers/-max-memory/-tile-cache-bytes (and prefetch tuning,
// which has no individual flag at all). An empty preset behaves like
// "balanced": today's flag defaults, unchanged.
type serverPreset struct {
	readers        int
	maxMemory      int64
	tileCacheBytes int64

	// Prefetch* have no individual flags (see server.Config's doc comments
	// for their zero-value/-1 semantics); a preset is the only way to tune
	// them from this binary today.
	prefetchWorkers  int
	prefetchQueue    int
	prefetchMaxTiles int
}

// serverPresetNames lists every defined preset name, comma-separated, for
// flag usage text and error messages.
const serverPresetNames = "embedded, balanced, high-traffic"

// resolveServerPreset returns name's concrete Config values. An empty name
// resolves identically to "balanced": readers/max-memory/tile-cache-bytes
// match this binary's existing flag defaults exactly, and the zero-value
// Prefetch* fields select server.Config's own defaults — so requesting no
// preset changes nothing.
func resolveServerPreset(name string) (serverPreset, error) {
	switch strings.TrimSpace(name) {
	case "", "balanced":
		return serverPreset{
			readers:        min(runtime.GOMAXPROCS(0), 8),
			maxMemory:      64 << 20,
			tileCacheBytes: server.DefaultTileCacheBytes,
		}, nil
	case "embedded":
		// A small recovery/edge host serving one artifact to a handful of
		// clients: minimal readers and cache footprint, predictive
		// prefetching disabled outright (PrefetchWorkers: -1).
		return serverPreset{
			readers:         1,
			maxMemory:       16 << 20,
			tileCacheBytes:  4 << 20,
			prefetchWorkers: -1,
		}, nil
	case "high-traffic":
		// A public-facing deployment fronting many concurrent clients:
		// more readers, a much larger tile cache, and more headroom for
		// route-prediction prefetch than server.Config's own defaults.
		return serverPreset{
			readers:          min(runtime.GOMAXPROCS(0), 16),
			maxMemory:        128 << 20,
			tileCacheBytes:   256 << 20,
			prefetchWorkers:  8,
			prefetchQueue:    2048,
			prefetchMaxTiles: 512,
		}, nil
	default:
		return serverPreset{}, fmt.Errorf("unknown preset %q (choose one of: %s)", name, serverPresetNames)
	}
}
