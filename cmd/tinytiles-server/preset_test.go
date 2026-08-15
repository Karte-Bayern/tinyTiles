package main

import (
	"runtime"
	"testing"

	"github.com/Karte-Bayern/tinyTiles/v2/server"
)

func TestResolveServerPresetKnownValues(t *testing.T) {
	tests := []struct {
		name string
		want serverPreset
	}{
		{"balanced", serverPreset{readers: min(runtime.GOMAXPROCS(0), 8), maxMemory: 64 << 20, tileCacheBytes: server.DefaultTileCacheBytes}},
		{"embedded", serverPreset{readers: 1, maxMemory: 16 << 20, tileCacheBytes: 4 << 20, prefetchWorkers: -1}},
		{"high-traffic", serverPreset{
			readers: min(runtime.GOMAXPROCS(0), 16), maxMemory: 128 << 20, tileCacheBytes: 256 << 20,
			prefetchWorkers: 8, prefetchQueue: 2048, prefetchMaxTiles: 512,
		}},
	}
	for _, test := range tests {
		got, err := resolveServerPreset(test.name)
		if err != nil {
			t.Fatalf("resolveServerPreset(%q): %v", test.name, err)
		}
		if got != test.want {
			t.Fatalf("resolveServerPreset(%q) = %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestResolveServerPresetEmptyMatchesBalanced(t *testing.T) {
	empty, err := resolveServerPreset("")
	if err != nil {
		t.Fatal(err)
	}
	balanced, err := resolveServerPreset("balanced")
	if err != nil {
		t.Fatal(err)
	}
	if empty != balanced {
		t.Fatalf("resolveServerPreset(\"\") = %+v, want it to match \"balanced\" = %+v", empty, balanced)
	}
}

func TestResolveServerPresetUnknownReturnsError(t *testing.T) {
	if _, err := resolveServerPreset("bogus"); err == nil {
		t.Fatal("resolveServerPreset(\"bogus\") = nil error, want an error")
	}
}

func TestResolveServerPresetEmbeddedDisablesPrefetch(t *testing.T) {
	embedded, err := resolveServerPreset("embedded")
	if err != nil {
		t.Fatal(err)
	}
	if embedded.prefetchWorkers != -1 {
		t.Fatalf("embedded preset PrefetchWorkers = %d, want -1 (disabled)", embedded.prefetchWorkers)
	}
}
