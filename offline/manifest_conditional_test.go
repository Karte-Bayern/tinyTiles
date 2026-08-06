package offline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestHTTPFetcherFetchManifestIfChangedRevalidates confirms the real
// conditional GET wiring: the server sees If-None-Match carrying the quoted
// known revision, and a 304 response is reported as unchanged without a
// manifest body.
func TestHTTPFetcherFetchManifestIfChangedRevalidates(t *testing.T) {
	var sawIfNoneMatch string
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		sawIfNoneMatch = request.Header.Get("If-None-Match")
		if sawIfNoneMatch == `"r1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"r1"`)
		_, _ = w.Write([]byte(`{"format_version":1,"dataset":"demo","revision":"r1","coordinate_system":"TMS","tile_url_template":"/tiles/{revision}/{z}/{x}/{y}"}`))
	}))
	defer server.Close()
	fetcher := &HTTPFetcher{ManifestURL: server.URL}

	manifest, unchanged, err := fetcher.FetchManifestIfChanged(context.Background(), "r1")
	if err != nil {
		t.Fatalf("FetchManifestIfChanged: %v", err)
	}
	if !unchanged {
		t.Fatalf("unchanged = false, manifest = %#v, want a 304", manifest)
	}
	if manifest != (Manifest{}) {
		t.Fatalf("unchanged response returned a non-zero manifest: %#v", manifest)
	}
	if requests != 1 || sawIfNoneMatch != `"r1"` {
		t.Fatalf("requests=%d If-None-Match=%q, want exactly one request carrying \"r1\"", requests, sawIfNoneMatch)
	}
}

func TestHTTPFetcherFetchManifestIfChangedReturnsFullBodyWhenChanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// The client believes the revision is "r1"; the server has since
		// published "r2" and must return the full body, not a 304.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"r2"`)
		_, _ = w.Write([]byte(`{"format_version":1,"dataset":"demo","revision":"r2","coordinate_system":"TMS","tile_url_template":"/tiles/{revision}/{z}/{x}/{y}"}`))
	}))
	defer server.Close()
	fetcher := &HTTPFetcher{ManifestURL: server.URL}

	manifest, unchanged, err := fetcher.FetchManifestIfChanged(context.Background(), "r1")
	if err != nil {
		t.Fatalf("FetchManifestIfChanged: %v", err)
	}
	if unchanged {
		t.Fatal("unchanged = true, want the new revision's full manifest")
	}
	if manifest.Revision != "r2" {
		t.Fatalf("manifest.Revision = %q, want r2", manifest.Revision)
	}
}

func TestHTTPFetcherFetchManifestIfChangedRejectsEmptyKnownRevision(t *testing.T) {
	fetcher := &HTTPFetcher{ManifestURL: "https://tiles.example.invalid/sync/manifest.json"}
	if _, _, err := fetcher.FetchManifestIfChanged(context.Background(), ""); err == nil {
		t.Fatal("empty known revision accepted")
	}
}

// conditionalFetcher wraps fakeFetcher to additionally implement
// ConditionalManifestFetcher, so Synchronizer.resolveManifest's conditional
// branch can be exercised without a real HTTP server.
type conditionalFetcher struct {
	*fakeFetcher
	mu                    sync.Mutex
	revalidateCalls       int
	lastKnownRevision     string
	fullManifestFetches   int
	forceUnchangedAgainst string // when equal to the requested knownRevision, report unchanged
}

func (f *conditionalFetcher) FetchManifest(ctx context.Context) (Manifest, error) {
	f.mu.Lock()
	f.fullManifestFetches++
	f.mu.Unlock()
	return f.fakeFetcher.FetchManifest(ctx)
}

func (f *conditionalFetcher) FetchManifestIfChanged(ctx context.Context, knownRevision string) (Manifest, bool, error) {
	f.mu.Lock()
	f.revalidateCalls++
	f.lastKnownRevision = knownRevision
	unchanged := knownRevision == f.forceUnchangedAgainst
	f.mu.Unlock()
	if unchanged {
		return Manifest{}, true, nil
	}
	manifest, err := f.fakeFetcher.FetchManifest(ctx)
	return manifest, false, err
}

var _ ConditionalManifestFetcher = (*conditionalFetcher)(nil)

func TestSynchronizerRevalidatesInsteadOfRefetchingUnchangedManifest(t *testing.T) {
	store := NewMemoryStore()
	fetcher := &conditionalFetcher{fakeFetcher: &fakeFetcher{manifest: testManifest("r1")}}
	request := SyncRequest{Dataset: "demo", Ranges: []TileRange{{Z: 6, XMin: 0, XMax: 0, YMin: 0, YMax: 0}}}

	synchronizer := &Synchronizer{Store: store, Fetcher: fetcher}
	if _, err := synchronizer.Sync(context.Background(), request); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if fetcher.revalidateCalls != 0 {
		t.Fatalf("first sync (no cached manifest yet) made %d conditional calls, want 0", fetcher.revalidateCalls)
	}
	if fetcher.fullManifestFetches != 1 {
		t.Fatalf("first sync made %d full fetches, want exactly 1", fetcher.fullManifestFetches)
	}

	// The dataset has not changed: the server would answer 304 for "r1".
	fetcher.forceUnchangedAgainst = "r1"
	result, err := synchronizer.Sync(context.Background(), request)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if fetcher.revalidateCalls != 1 || fetcher.lastKnownRevision != "r1" {
		t.Fatalf("second sync revalidateCalls=%d lastKnownRevision=%q, want 1 call for r1", fetcher.revalidateCalls, fetcher.lastKnownRevision)
	}
	if fetcher.fullManifestFetches != 1 {
		t.Fatalf("second sync performed %d full manifest fetches, want the same 1 as before (revalidation replaced it)", fetcher.fullManifestFetches)
	}
	if result.Revision != "r1" || result.Reused != 1 || result.Downloaded != 0 {
		t.Fatalf("second sync result = %#v, want revision r1 fully reused from cache", result)
	}
}

func TestSynchronizerFetchesFullManifestWhenRevisionChanged(t *testing.T) {
	store := NewMemoryStore()
	fetcher := &conditionalFetcher{fakeFetcher: &fakeFetcher{manifest: testManifest("r1")}}
	request := SyncRequest{Dataset: "demo", Ranges: []TileRange{{Z: 6, XMin: 0, XMax: 0, YMin: 0, YMax: 0}}}
	synchronizer := &Synchronizer{Store: store, Fetcher: fetcher}
	if _, err := synchronizer.Sync(context.Background(), request); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Publish a new revision. forceUnchangedAgainst stays "" (default), so
	// FetchManifestIfChanged("r1") reports changed and returns the full body.
	fetcher.mu.Lock()
	fetcher.manifest = testManifest("r2")
	fetcher.mu.Unlock()
	result, err := synchronizer.Sync(context.Background(), request)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if fetcher.revalidateCalls != 1 || fetcher.lastKnownRevision != "r1" {
		t.Fatalf("revalidateCalls=%d lastKnownRevision=%q, want 1 call for r1", fetcher.revalidateCalls, fetcher.lastKnownRevision)
	}
	if result.Revision != "r2" || result.Downloaded != 1 {
		t.Fatalf("result = %#v, want the new revision downloaded", result)
	}
}

// TestSynchronizerSkipsConditionalPathWithoutDatasetOrPriorCache confirms
// resolveManifest falls back to the ordinary unconditional FetchManifest —
// unchanged behavior from before this feature existed — whenever it cannot
// safely attempt revalidation.
func TestSynchronizerSkipsConditionalPathWithoutDatasetOrPriorCache(t *testing.T) {
	t.Run("no dataset named in the request", func(t *testing.T) {
		store := NewMemoryStore()
		fetcher := &conditionalFetcher{fakeFetcher: &fakeFetcher{manifest: testManifest("r1")}}
		synchronizer := &Synchronizer{Store: store, Fetcher: fetcher}
		if _, err := synchronizer.Sync(context.Background(), SyncRequest{Ranges: []TileRange{{Z: 6, XMin: 0, XMax: 0, YMin: 0, YMax: 0}}}); err != nil {
			t.Fatal(err)
		}
		if fetcher.revalidateCalls != 0 || fetcher.fullManifestFetches != 1 {
			t.Fatalf("revalidateCalls=%d fullManifestFetches=%d, want the unconditional path with no Dataset set", fetcher.revalidateCalls, fetcher.fullManifestFetches)
		}
	})
	t.Run("fetcher does not support conditional requests", func(t *testing.T) {
		// A plain fakeFetcher (unlike conditionalFetcher above) implements only
		// Fetcher, not ConditionalManifestFetcher. resolveManifest must still
		// complete both syncs correctly by falling back to unconditional
		// FetchManifest every time — the exact pre-existing behavior.
		store := NewMemoryStore()
		fetcher := &fakeFetcher{manifest: testManifest("r1")}
		synchronizer := &Synchronizer{Store: store, Fetcher: fetcher}
		request := SyncRequest{Dataset: "demo", Ranges: []TileRange{{Z: 6, XMin: 0, XMax: 0, YMin: 0, YMax: 0}}}
		first, err := synchronizer.Sync(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if first.Downloaded != 1 || first.Reused != 0 {
			t.Fatalf("first sync = %#v, want one freshly downloaded tile", first)
		}
		second, err := synchronizer.Sync(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if second.Downloaded != 0 || second.Reused != 1 {
			t.Fatalf("second sync = %#v, want the tile reused from a warm cache", second)
		}
	})
}
