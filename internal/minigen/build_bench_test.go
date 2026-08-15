package minigen

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// benchmarkFixturePBF writes a synthetic PBF with a grid of nodes and a mix
// of road/building/water ways, large enough that the per-zoom full-file
// rescan this package used to do (one zlib-decompress-and-decode pass per
// zoom level) shows up clearly against a single feature-collection pass.
func benchmarkFixturePBF(b *testing.B, dir string, gridSize int) string {
	b.Helper()
	table := []string{""}
	index := map[string]uint64{}
	get := func(s string) uint64 {
		if i, ok := index[s]; ok {
			return i
		}
		i := uint64(len(table))
		table = append(table, s)
		index[s] = i
		return i
	}

	var ids, lats, lons []int64
	nodeID := func(row, col int) int64 { return int64(row*gridSize + col + 1) }
	for row := 0; row < gridSize; row++ {
		for col := 0; col < gridSize; col++ {
			ids = append(ids, nodeID(row, col))
			lats = append(lats, int64(math.Round((48.0+float64(row)*0.01)*1e7)))
			lons = append(lons, int64(math.Round((11.0+float64(col)*0.01)*1e7)))
		}
	}
	dense := regressionMessage(
		regressionBytesField(1, regressionPacked(regressionDeltaZigZag(ids)...)),
		regressionBytesField(8, regressionPacked(regressionDeltaZigZag(lats)...)),
		regressionBytesField(9, regressionPacked(regressionDeltaZigZag(lons)...)),
	)

	highwayKey := get("highway")
	buildingKey := get("building")
	naturalKey := get("natural")
	motorway := get("motorway")
	residential := get("residential")
	yes := get("yes")
	water := get("water")

	var group []byte
	group = append(group, regressionBytesField(2, dense)...)
	for row := 0; row < gridSize-1; row++ {
		for col := 0; col < gridSize-1; col++ {
			// A short line way per grid cell, alternating road classes.
			key, val := highwayKey, residential
			if (row+col)%5 == 0 {
				val = motorway
			}
			line := regressionMessage(
				regressionBytesField(2, regressionPacked(key)),
				regressionBytesField(3, regressionPacked(val)),
				regressionBytesField(8, regressionPacked(regressionDeltaZigZag([]int64{nodeID(row, col), nodeID(row, col+1)})...)),
			)
			group = append(group, regressionBytesField(3, line)...)

			// A small closed ring every few cells, alternating building/water.
			if (row+col)%3 == 0 {
				key, val := buildingKey, yes
				if (row+col)%9 == 0 {
					key, val = naturalKey, water
				}
				ring := regressionMessage(
					regressionBytesField(2, regressionPacked(key)),
					regressionBytesField(3, regressionPacked(val)),
					regressionBytesField(8, regressionPacked(regressionDeltaZigZag([]int64{
						nodeID(row, col), nodeID(row, col+1), nodeID(row+1, col+1), nodeID(row+1, col), nodeID(row, col),
					})...)),
				)
				group = append(group, regressionBytesField(3, ring)...)
			}
		}
	}

	var stringTable []byte
	for _, s := range table {
		stringTable = append(stringTable, regressionBytesField(1, []byte(s))...)
	}
	block := regressionMessage(
		regressionBytesField(1, stringTable),
		regressionBytesField(2, group),
	)
	blob := regressionMessage(regressionBytesField(1, block))
	header := regressionMessage(
		regressionBytesField(1, []byte("OSMData")),
		regressionVarintField(3, uint64(len(blob))),
	)
	var contents []byte
	var size [4]byte
	size[0], size[1], size[2], size[3] = byte(len(header)>>24), byte(len(header)>>16), byte(len(header)>>8), byte(len(header))
	contents = append(contents, size[:]...)
	contents = append(contents, header...)
	contents = append(contents, blob...)

	path := filepath.Join(dir, "bench.osm.pbf")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkBuild measures Build() across a wide zoom range (5..16, 12 levels)
// at increasing Concurrency, on a synthetic fixture large enough to make the
// difference between rescanning the PBF per zoom and decoding it once
// visible. Run with: go test ./internal/minigen/... -bench=BenchmarkBuild -benchmem
func BenchmarkBuild(b *testing.B) {
	dir := b.TempDir()
	pbf := benchmarkFixturePBF(b, dir, 60)

	for _, concurrency := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("concurrency=%d", concurrency), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				output := filepath.Join(dir, fmt.Sprintf("out-%d-%d.tiles", concurrency, i))
				if _, err := Build(context.Background(), Config{
					PBFInputs:   []string{pbf},
					Output:      output,
					MinZoom:     5,
					MaxZoom:     16,
					Concurrency: concurrency,
				}); err != nil {
					b.Fatal(err)
				}
				os.Remove(output)
			}
		})
	}
}
