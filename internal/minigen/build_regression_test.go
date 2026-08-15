package minigen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This file pins down Build()'s exact output on a small synthetic PBF fixture
// spanning several road/area classes and a wide zoom range. It exists to
// prove that collapsing Build()'s per-zoom full-file rescans into a single
// feature-collection pass (and parallelizing tile encoding across
// Config.Concurrency) does not change a single byte of a published tileset —
// only how fast it's produced. The expected values below were captured by
// running this test against the pre-refactor implementation.

// regressionNode is one synthetic OSM node used to build the fixture PBF.
type regressionNode struct {
	id       int64
	lon, lat float64
}

// regressionWay is one synthetic OSM way used to build the fixture PBF.
type regressionWay struct {
	nodeIDs []int64
	tags    map[string]string
}

func regressionFixtureNodes() []regressionNode {
	return []regressionNode{
		{1, 11.50, 48.10},
		{2, 11.51, 48.10},
		{3, 11.55, 48.14},
		{4, 11.60, 48.14},
		{5, 11.50, 48.20},
		{6, 11.52, 48.20},
		{7, 11.52, 48.22},
		{8, 11.50, 48.22},
	}
}

func regressionFixtureWays() []regressionWay {
	return []regressionWay{
		// motorway: minZoom 5, a short line.
		{[]int64{1, 2}, map[string]string{"highway": "motorway"}},
		// residential: minZoom 11, a short line.
		{[]int64{3, 4}, map[string]string{"highway": "residential"}},
		// building: minZoom 14, a closed 4-point ring.
		{[]int64{5, 6, 7, 8, 5}, map[string]string{"building": "yes"}},
		// water: minZoom 8, a closed 4-point ring reusing nodes from other ways.
		{[]int64{2, 4, 6, 8, 2}, map[string]string{"natural": "water"}},
	}
}

// writeRegressionFixturePBF writes a minimal, single-block PBF file (one
// dense-node group plus way primitives) built from the fixture above.
func writeRegressionFixturePBF(t *testing.T, dir, name string) string {
	t.Helper()
	return writeRegressionFixtureSubset(t, dir, name, regressionFixtureNodes(), regressionFixtureWays())
}

func regressionDeltaZigZag(values []int64) []uint64 {
	out := make([]uint64, len(values))
	var prev int64
	for i, v := range values {
		out[i] = regressionZigZag(v - prev)
		prev = v
	}
	return out
}

func regressionZigZag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

func regressionMessage(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func regressionBytesField(field int, value []byte) []byte {
	out := regressionAppendVarint(nil, uint64(field<<3|2))
	out = regressionAppendVarint(out, uint64(len(value)))
	return append(out, value...)
}

func regressionVarintField(field int, value uint64) []byte {
	out := regressionAppendVarint(nil, uint64(field<<3))
	return regressionAppendVarint(out, value)
}

func regressionPacked(values ...uint64) []byte {
	var out []byte
	for _, v := range values {
		out = regressionAppendVarint(out, v)
	}
	return out
}

func regressionAppendVarint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// regressionDigest builds a stable, order-independent summary of every tile
// Build() wrote: a sorted list of "z/x/y sha256(data)" lines, hashed together.
// Sorting first means the digest does not depend on the order tiles were
// written in, only on which (z,x,y)->data pairs exist.
func regressionDigest(t *testing.T, path string) string {
	t.Helper()
	stream, err := OpenTileStream(path)
	if err != nil {
		t.Fatalf("open tile stream: %v", err)
	}
	var lines []string
	err = stream.Scan(context.Background(), func(z, x, y int, data []byte) error {
		sum := sha256.Sum256(data)
		lines = append(lines, itoa(z)+"/"+itoa(x)+"/"+itoa(y)+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("scan tile stream: %v", err)
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, line := range lines {
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// regressionBuild runs Build() against the fixture PBF with the given
// concurrency and returns the result plus a content digest of every tile
// written.
func regressionBuild(t *testing.T, concurrency int) (Result, string) {
	t.Helper()
	dir := t.TempDir()
	pbf := writeRegressionFixturePBF(t, dir, "fixture.osm.pbf")
	output := filepath.Join(dir, "out.tiles")
	result, err := Build(context.Background(), Config{
		PBFInputs:   []string{pbf},
		Output:      output,
		MinZoom:     5,
		MaxZoom:     14,
		Concurrency: concurrency,
	})
	if err != nil {
		t.Fatalf("Build (concurrency=%d): %v", concurrency, err)
	}
	return result, regressionDigest(t, output)
}

// TestBuildSinglePassRegression pins Build()'s exact output for the fixture
// above at MinZoom=5..MaxZoom=14, Concurrency=1. It must keep passing,
// unmodified, across the switch from per-zoom PBF rescans to a single
// feature-collection pass — that equivalence is the correctness gate for the
// whole optimization.
func TestBuildSinglePassRegression(t *testing.T) {
	const wantRoads = 14 // motorway (zooms 5-14 = 10) + residential (zooms 11-14 = 4)
	const wantTiles = 71
	const wantDigest = "3f20710e8bed45eea5590195b02cde808824f1d762f5edb95f4a421304277d75"

	result, digest := regressionBuild(t, 1)
	if result.Roads != wantRoads {
		t.Fatalf("Roads = %d, want %d", result.Roads, wantRoads)
	}
	if result.Tiles != wantTiles {
		t.Fatalf("Tiles = %d, want %d", result.Tiles, wantTiles)
	}
	if digest != wantDigest {
		t.Fatalf("tile content digest = %s, want %s", digest, wantDigest)
	}
	const wantBounds = "11.500000,48.100000,11.600000,48.220000"
	if got := result.Bounds.String(); got != wantBounds {
		t.Fatalf("Bounds = %s, want %s", got, wantBounds)
	}
}

// TestBuildConcurrencyDeterminism proves parallel tile encoding produces
// byte-identical output to sequential encoding: only the write order inside
// writeZoom may parallelize, never the resulting bytes or their (z,x,y) keys.
func TestBuildConcurrencyDeterminism(t *testing.T) {
	base, baseDigest := regressionBuild(t, 1)
	for _, concurrency := range []int{2, 4, 8} {
		result, digest := regressionBuild(t, concurrency)
		if !regressionResultsEqual(result, base) {
			t.Fatalf("concurrency=%d Result = %+v, want %+v", concurrency, result, base)
		}
		if digest != baseDigest {
			t.Fatalf("concurrency=%d tile content digest = %s, want %s", concurrency, digest, baseDigest)
		}
	}
}

// TestBuildMultiInputMatchesSingleInput proves that splitting the fixture's
// ways across two PBF inputs produces the same result as a single input
// covering everything, whether or not per-input scanning parallelizes.
func TestBuildMultiInputMatchesSingleInput(t *testing.T) {
	dir := t.TempDir()

	singlePBF := writeRegressionFixturePBF(t, dir, "single.osm.pbf")
	singleOut := filepath.Join(dir, "single.tiles")
	singleResult, err := Build(context.Background(), Config{
		PBFInputs: []string{singlePBF},
		Output:    singleOut,
		MinZoom:   5,
		MaxZoom:   14,
	})
	if err != nil {
		t.Fatalf("single-input Build: %v", err)
	}
	singleDigest := regressionDigest(t, singleOut)

	splitDir := t.TempDir()
	partA := writeRegressionFixtureSubset(t, splitDir, "a.osm.pbf", regressionFixtureNodes(), regressionFixtureWays()[:2])
	partB := writeRegressionFixtureSubset(t, splitDir, "b.osm.pbf", regressionFixtureNodes(), regressionFixtureWays()[2:])
	splitOut := filepath.Join(splitDir, "split.tiles")
	splitResult, err := Build(context.Background(), Config{
		PBFInputs:   []string{partA, partB},
		Output:      splitOut,
		MinZoom:     5,
		MaxZoom:     14,
		Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("multi-input Build: %v", err)
	}
	splitDigest := regressionDigest(t, splitOut)

	if !regressionResultsEqual(splitResult, singleResult) {
		t.Fatalf("multi-input Result = %+v, want %+v", splitResult, singleResult)
	}
	if splitDigest != singleDigest {
		t.Fatalf("multi-input tile content digest = %s, want %s", splitDigest, singleDigest)
	}
}

// regressionResultsEqual compares the fields of Result exercised by these
// fixtures directly, since Result embeds a []PostalFeature slice and is not
// comparable with ==.
func regressionResultsEqual(a, b Result) bool {
	return a.Roads == b.Roads && a.Tiles == b.Tiles && a.Bounds == b.Bounds && len(a.PostalCodes) == len(b.PostalCodes)
}

// writeRegressionFixtureSubset writes a fixture PBF containing every node
// (so cross-file node references still resolve) but only the given subset of
// ways, letting a test split the fixture's ways across multiple PBF inputs.
func writeRegressionFixtureSubset(t *testing.T, dir, name string, nodes []regressionNode, ways []regressionWay) string {
	t.Helper()
	table := []string{""}
	index := func(s string) uint64 {
		for i, existing := range table {
			if existing == s {
				return uint64(i)
			}
		}
		table = append(table, s)
		return uint64(len(table) - 1)
	}

	var ids, lats, lons []int64
	for _, n := range nodes {
		ids = append(ids, n.id)
		lats = append(lats, int64(math.Round(n.lat*1e7)))
		lons = append(lons, int64(math.Round(n.lon*1e7)))
	}
	dense := regressionMessage(
		regressionBytesField(1, regressionPacked(regressionDeltaZigZag(ids)...)),
		regressionBytesField(8, regressionPacked(regressionDeltaZigZag(lats)...)),
		regressionBytesField(9, regressionPacked(regressionDeltaZigZag(lons)...)),
	)

	var wayMessages [][]byte
	for _, w := range ways {
		var keys, vals []uint64
		for k, v := range w.tags {
			keys = append(keys, index(k))
			vals = append(vals, index(v))
		}
		nodeIDs64 := make([]int64, len(w.nodeIDs))
		copy(nodeIDs64, w.nodeIDs)
		wayMessages = append(wayMessages, regressionMessage(
			regressionBytesField(2, regressionPacked(keys...)),
			regressionBytesField(3, regressionPacked(vals...)),
			regressionBytesField(8, regressionPacked(regressionDeltaZigZag(nodeIDs64)...)),
		))
	}

	var group []byte
	group = append(group, regressionBytesField(2, dense)...)
	for _, w := range wayMessages {
		group = append(group, regressionBytesField(3, w)...)
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
	size[0] = byte(len(header) >> 24)
	size[1] = byte(len(header) >> 16)
	size[2] = byte(len(header) >> 8)
	size[3] = byte(len(header))
	contents = append(contents, size[:]...)
	contents = append(contents, header...)
	contents = append(contents, blob...)

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
