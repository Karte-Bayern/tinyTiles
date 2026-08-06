//go:build !js || !wasm

package offline

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func BenchmarkManifestTileURL(b *testing.B) {
	manifest := Manifest{
		FormatVersion:    ProtocolVersion,
		Dataset:          "benchmark",
		Revision:         "2026-08-06T12:34:56Z",
		CoordinateSystem: "TMS",
		TileURLTemplate:  "https://tiles.example.invalid/tiles/{revision}/{z}/{x}/{y}.mvt",
	}
	key := TileKey{Z: 14, X: 8567, Y: 5461}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		url, err := manifest.TileURL(key)
		if err != nil || url == "" {
			b.Fatalf("TileURL = %q, %v", url, err)
		}
	}
}

func BenchmarkHTTPFetcherFetchTile(b *testing.B) {
	payload := makeBenchmarkTileBytes(8 << 10)
	manifest := Manifest{
		FormatVersion:    ProtocolVersion,
		Dataset:          "benchmark",
		Revision:         "2026-08-06T12:34:56Z",
		CoordinateSystem: "TMS",
		TileURLTemplate:  "https://tiles.example.invalid/tiles/{revision}/{z}/{x}/{y}.mvt",
	}
	key := TileKey{Z: 14, X: 8567, Y: 5461}
	fetcher := &HTTPFetcher{Client: &http.Client{Transport: benchmarkRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(payload)),
			Header: http.Header{
				HeaderTileChecksum: []string{Checksum(payload)},
			},
			Body: io.NopCloser(bytes.NewReader(payload)),
		}, nil
	})}}
	for _, benchmark := range []struct {
		name  string
		fetch func() (Tile, error)
	}{
		{name: "validated", fetch: func() (Tile, error) {
			return fetcher.FetchTile(context.Background(), manifest, key)
		}},
		{name: "prevalidated", fetch: func() (Tile, error) {
			return fetcher.fetchVerifiedTile(context.Background(), manifest, key)
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tile, err := benchmark.fetch()
				if err != nil || len(tile.Data) != len(payload) {
					b.Fatalf("FetchTile bytes=%d err=%v", len(tile.Data), err)
				}
			}
		})
	}
}

type benchmarkRoundTripper func(*http.Request) (*http.Response, error)

func (fn benchmarkRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
