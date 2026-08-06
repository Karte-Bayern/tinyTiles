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
		memory      = flag.Int64("max-memory", 16<<20, "per-reader tinySQL cache budget in bytes (0 uses default)")
		tileCache   = flag.Int64("tile-cache-bytes", server.DefaultTileCacheBytes, "immutable tile cache budget in bytes (-1 disables)")
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
	defer func() {
		if err := dataset.Close(); err != nil {
			log.Printf("close dataset: %v", err)
		}
	}()
	handler, err := server.New(server.Config{Dataset: dataset, DatasetID: *datasetID, PublicBase: *publicBase, CORSOrigin: *corsOrigin, TileCacheBytes: *tileCache})
	if err != nil {
		log.Fatal(err)
	}
	defer handler.Close()
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
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

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
