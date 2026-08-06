// tinytiles-server is the small standalone HTTP application for a published
// .ttiles artifact. Embedding applications should import tinyTiles and mount
// server.XYZHandler or server.Handler instead of spawning this binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tinytiles "github.com/Karte-Bayern/tinyTiles"
	"github.com/Karte-Bayern/tinyTiles/server"
)

// version is replaced with a SemVer tag by the release build. It is exposed
// before opening an artifact so deployment tooling can identify a binary
// without requiring its production configuration.
var version = "v1.0.0-dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		artifact    = flag.String("artifact", "", "published dataset.ttiles directory")
		addr        = flag.String("addr", ":8080", "listen address")
		datasetID   = flag.String("dataset", "", "stable dataset identifier for offline sync")
		publicBase  = flag.String("public-base", "", "canonical http(s) base URL; defaults to request origin")
		corsOrigin  = flag.String("cors", "", "optional CORS allow-origin")
		readers     = flag.Int("readers", min(runtime.GOMAXPROCS(0), 8), "independent artifact readers")
		// 64 MiB matches docs/benchmark-results-berlin-2026-08-06.md's pool/cache
		// sweep: it is the smallest per-reader budget that cleared the tail
		// latencies measured at 16/32 MiB (e.g. 8-reader spatial-2x2 p95 dropped
		// from 284.9us to 83.25us) on that regional-sized fixture.
		memory      = flag.Int64("max-memory", 64<<20, "per-reader tinySQL cache budget in bytes (0 uses default)")
		tileCache   = flag.Int64("tile-cache-bytes", server.DefaultTileCacheBytes, "immutable tile cache budget in bytes (-1 disables)")
		demEncoding = flag.String("dem-encoding", "", "declare a raster tileset as elevation data: terrarium, mapbox or custom")
	)
	flag.Parse()
	if *showVersion {
		fmt.Fprintln(os.Stdout, version)
		return
	}
	if *artifact == "" || *datasetID == "" || *readers < 1 || *memory < 0 || *tileCache < -1 {
		fmt.Fprintln(os.Stderr, "usage: tinytiles-server -artifact dataset.ttiles/ -dataset name [-addr :8080]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataset, err := tinytiles.Open(ctx, *artifact, tinytiles.OpenOptions{Readers: *readers, MaxMemoryBytes: *memory})
	if err != nil {
		log.Fatal(err)
	}
	var current atomic.Pointer[tinytiles.Dataset]
	current.Store(dataset)
	handler, err := server.New(server.Config{Dataset: dataset, DatasetID: *datasetID, PublicBase: *publicBase, CORSOrigin: *corsOrigin, TileCacheBytes: *tileCache, DEMEncoding: *demEncoding})
	if err != nil {
		log.Fatal(err)
	}

	// A SIGHUP reload lets an operator publish a new revision with
	// `tinytiles import --replace` and pick it up here without dropping
	// connections: reopening the same artifact path sees the new revision
	// once the atomic rename has completed, and SwapDataset moves the running
	// server onto it. A failed reopen or an invalid artifact is logged and
	// leaves the process serving its current revision; it never crashes the
	// server on a bad reload.
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)
	var reloader sync.WaitGroup
	reloader.Add(1)
	go func() {
		defer reloader.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reload:
				reloadDataset(ctx, handler, *artifact, *readers, *memory, &current)
			}
		}
	}()
	// Reversed shutdown order matters: wait for any in-progress reload to
	// finish and stop touching `current` before deciding which Dataset is
	// actually current, then stop background workers, then close it.
	defer func() {
		if ds := current.Load(); ds != nil {
			if err := ds.Close(); err != nil {
				log.Printf("close dataset: %v", err)
			}
		}
	}()
	defer handler.Close()
	defer reloader.Wait()

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout bounds how long a single response write may take,
		// resetting per request so it never penalizes a long-lived keep-alive
		// connection. Without it, a client that stalls mid-download — reading
		// a large raster tile over a very slow or dropped connection — holds
		// its goroutine, tinySQL reader borrow and tile cache reference open
		// indefinitely. 30s clears every tile observed in practice (the
		// largest Berlin z12 fixture tile, 482,657 bytes, needs under 10s even
		// at a pessimistic 50 KB/s) with headroom to spare.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}()
	log.Printf("tinyTiles serving %q on %s (XYZ /tiles, TMS sync /sync/manifest.json)", *datasetID, *addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// reloadDataset reopens artifactPath and, on success, atomically swaps handler
// onto the freshly opened Dataset before closing the one it replaced.
func reloadDataset(ctx context.Context, handler *server.Server, artifactPath string, readers int, memory int64, current *atomic.Pointer[tinytiles.Dataset]) {
	next, err := tinytiles.Open(ctx, artifactPath, tinytiles.OpenOptions{Readers: readers, MaxMemoryBytes: memory})
	if err != nil {
		log.Printf("reload %q: open: %v (still serving previous revision)", artifactPath, err)
		return
	}
	previous, err := handler.SwapDataset(next)
	if err != nil {
		log.Printf("reload %q: swap: %v (still serving previous revision)", artifactPath, err)
		if closeErr := next.Close(); closeErr != nil {
			log.Printf("reload %q: close rejected dataset: %v", artifactPath, closeErr)
		}
		return
	}
	current.Store(next)
	if err := previous.Close(); err != nil {
		log.Printf("reload %q: close previous revision: %v", artifactPath, err)
	}
	log.Printf("reload %q: now serving newly opened revision", artifactPath)
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
