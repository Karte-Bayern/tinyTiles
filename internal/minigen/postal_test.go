package minigen

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePostalPBF builds a tiny synthetic PBF: a 4-node square, one closed
// way over it (id 100, no tags), and one boundary=postal_code relation
// (id 500) with that way as its "outer" member. Real extracts keep node,
// way and relation primitives in separate groups; mixing them in one group
// here is still valid PBF and exercises both scan passes against the same
// file.
func writePostalPBF(t *testing.T) string {
	t.Helper()
	// table: 0 unused, 1 boundary, 2 postal_code, 3 12345, 4 outer, 5 building, 6 yes
	strings := message(
		bytesField(1, []byte{}),
		bytesField(1, []byte("boundary")),
		bytesField(1, []byte("postal_code")),
		bytesField(1, []byte("12345")),
		bytesField(1, []byte("outer")),
		bytesField(1, []byte("building")),
		bytesField(1, []byte("yes")),
	)
	dense := message(
		bytesField(1, packed(zigzagEncode(1), zigzagEncode(1), zigzagEncode(1), zigzagEncode(1))),
		bytesField(8, packed(zigzagEncode(500_000_000), zigzagEncode(0), zigzagEncode(100_000), zigzagEncode(0))),
		bytesField(9, packed(zigzagEncode(80_000_000), zigzagEncode(100_000), zigzagEncode(0), zigzagEncode(-100_000))),
	)
	way := message(
		varintField(1, 100),
		bytesField(2, packed(5)),
		bytesField(3, packed(6)),
		bytesField(8, packed(zigzagEncode(1), zigzagEncode(1), zigzagEncode(1), zigzagEncode(1), zigzagEncode(-3))),
	)
	rel := message(
		varintField(1, 500),
		bytesField(2, packed(1, 2)),
		bytesField(3, packed(2, 3)),
		bytesField(8, packed(4)),
		bytesField(9, packed(zigzagEncode(100))),
		bytesField(10, packed(1)),
	)
	group := message(bytesField(2, dense), bytesField(3, way), bytesField(4, rel))
	block := message(bytesField(1, strings), bytesField(2, group))
	blob := message(bytesField(1, block))
	header := message(bytesField(1, []byte("OSMData")), varintField(3, uint64(len(blob))))
	file := make([]byte, 4)
	binary.BigEndian.PutUint32(file, uint32(len(header)))
	file = append(file, header...)
	file = append(file, blob...)
	path := filepath.Join(t.TempDir(), "postal.osm.pbf")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollectPostalFeaturesAssemblesRelation(t *testing.T) {
	path := writePostalPBF(t)
	features, err := collectPostalFeatures(t.Context(), []string{path}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 1 {
		t.Fatalf("features = %#v", features)
	}
	f := features[0]
	if f.Code != "12345" {
		t.Errorf("Code = %q, want 12345", f.Code)
	}
	if len(f.Geometry) != 1 || len(f.Geometry[0].Rings) != 1 {
		t.Fatalf("Geometry = %#v", f.Geometry)
	}
	ring := f.Geometry[0].Rings[0]
	if len(ring) != 5 || ring[0] != ring[len(ring)-1] {
		t.Fatalf("exterior ring = %v", ring)
	}
}

func TestBuildWithPostalCodesAddsLayer(t *testing.T) {
	path := writePostalPBF(t)
	out := filepath.Join(t.TempDir(), "out.tiles")
	result, err := Build(t.Context(), Config{
		PBFInputs:   []string{path},
		Output:      out,
		MinZoom:     postalMinZoom,
		MaxZoom:     postalMinZoom,
		PostalCodes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PostalCodes) != 1 || result.PostalCodes[0].Code != "12345" {
		t.Fatalf("result.PostalCodes = %#v", result.PostalCodes)
	}

	stream, err := OpenTileStream(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stream.Metadata()["json"], `"id":"postal_code"`) {
		t.Errorf("metadata json missing postal_code vector layer: %s", stream.Metadata()["json"])
	}
	found := false
	if err := stream.Scan(t.Context(), func(z, x, y int, data []byte) error {
		found = found || len(data) > 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected at least one non-empty tile")
	}
}

func TestBuildWithoutPostalCodesOmitsLayer(t *testing.T) {
	path := writePostalPBF(t)
	out := filepath.Join(t.TempDir(), "out.tiles")
	result, err := Build(t.Context(), Config{PBFInputs: []string{path}, Output: out, MinZoom: postalMinZoom, MaxZoom: postalMinZoom})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PostalCodes) != 0 {
		t.Fatalf("expected no postal codes when disabled, got %#v", result.PostalCodes)
	}
	stream, err := OpenTileStream(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stream.Metadata()["json"], "postal_code") {
		t.Errorf("metadata should not mention postal_code when disabled: %s", stream.Metadata()["json"])
	}
}
