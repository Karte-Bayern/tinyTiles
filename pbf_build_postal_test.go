//go:build !js && !wasm && !baremetal

package tinytiles

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writePBFBuildPostalFixture is writePBFBuildFixture plus one
// boundary=postal_code relation over a second, building-tagged way (so the
// file still has an ordinary renderable feature and BuildPBF's ordinary
// bounds check passes independent of postal-code assembly).
func writePBFBuildPostalFixture(t *testing.T, dir, name string) string {
	t.Helper()
	// table: 0 unused, 1 highway, 2 residential, 3 boundary, 4 postal_code,
	// 5 12345, 6 outer, 7 building, 8 yes
	stringsTable := pbfBuildMessage(
		pbfBuildBytesField(1, []byte{}),
		pbfBuildBytesField(1, []byte("highway")),
		pbfBuildBytesField(1, []byte("residential")),
		pbfBuildBytesField(1, []byte("boundary")),
		pbfBuildBytesField(1, []byte("postal_code")),
		pbfBuildBytesField(1, []byte("12345")),
		pbfBuildBytesField(1, []byte("outer")),
		pbfBuildBytesField(1, []byte("building")),
		pbfBuildBytesField(1, []byte("yes")),
	)
	// Node ids 10,11 (highway endpoints), then 20,21,22,23 (postal square).
	dense := pbfBuildMessage(
		pbfBuildBytesField(1, pbfBuildPacked(pbfBuildZigZag(10), pbfBuildZigZag(1), pbfBuildZigZag(9), pbfBuildZigZag(1), pbfBuildZigZag(1), pbfBuildZigZag(1))),
		pbfBuildBytesField(8, pbfBuildPacked(pbfBuildZigZag(500_000_000), pbfBuildZigZag(10), pbfBuildZigZag(0), pbfBuildZigZag(0), pbfBuildZigZag(100_000), pbfBuildZigZag(0))),
		pbfBuildBytesField(9, pbfBuildPacked(pbfBuildZigZag(80_000_000), pbfBuildZigZag(10), pbfBuildZigZag(0), pbfBuildZigZag(100_000), pbfBuildZigZag(0), pbfBuildZigZag(-100_000))),
	)
	highwayWay := pbfBuildMessage(
		pbfBuildVarintField(1, 1),
		pbfBuildBytesField(2, pbfBuildPacked(1)),
		pbfBuildBytesField(3, pbfBuildPacked(2)),
		pbfBuildBytesField(8, pbfBuildPacked(pbfBuildZigZag(10), pbfBuildZigZag(1))),
	)
	postalWay := pbfBuildMessage(
		pbfBuildVarintField(1, 100),
		pbfBuildBytesField(2, pbfBuildPacked(7)),
		pbfBuildBytesField(3, pbfBuildPacked(8)),
		pbfBuildBytesField(8, pbfBuildPacked(pbfBuildZigZag(20), pbfBuildZigZag(1), pbfBuildZigZag(1), pbfBuildZigZag(1), pbfBuildZigZag(-3))),
	)
	relation := pbfBuildMessage(
		pbfBuildVarintField(1, 500),
		pbfBuildBytesField(2, pbfBuildPacked(3, 4)),
		pbfBuildBytesField(3, pbfBuildPacked(4, 5)),
		pbfBuildBytesField(8, pbfBuildPacked(6)),
		pbfBuildBytesField(9, pbfBuildPacked(pbfBuildZigZag(100))),
		pbfBuildBytesField(10, pbfBuildPacked(1)),
	)
	block := pbfBuildMessage(
		pbfBuildBytesField(1, stringsTable),
		pbfBuildBytesField(2, pbfBuildMessage(
			pbfBuildBytesField(2, dense),
			pbfBuildBytesField(3, highwayWay),
			pbfBuildBytesField(3, postalWay),
			pbfBuildBytesField(4, relation),
		)),
	)
	blob := pbfBuildMessage(pbfBuildBytesField(1, block))
	header := pbfBuildMessage(
		pbfBuildBytesField(1, []byte("OSMData")),
		pbfBuildVarintField(3, uint64(len(blob))),
	)
	contents := make([]byte, 4)
	binary.BigEndian.PutUint32(contents, uint32(len(header)))
	contents = append(contents, header...)
	contents = append(contents, blob...)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildPBFWritesPostalCodesSidecar(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildPostalFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")
	result, err := BuildPBF(t.Context(), PBFBuildOptions{
		PBFInputs:      []string{pbf},
		ArtifactPath:   artifact,
		MinZoom:        14,
		MaxZoom:        14,
		Concurrency:    1,
		BatchSize:      1,
		MaxMemoryBytes: 64 << 20,
		MinFreeBytes:   1,
		PostalCodes:    true,
	})
	if err != nil {
		t.Fatalf("BuildPBF: %v", err)
	}
	if result.PostalCodeCount != 1 {
		t.Fatalf("PostalCodeCount = %d, want 1", result.PostalCodeCount)
	}
	wantSidecar := filepath.Join(dir, "region.postcodes.geojson")
	if result.PostalCodesPath != wantSidecar {
		t.Fatalf("PostalCodesPath = %q, want %q", result.PostalCodesPath, wantSidecar)
	}
	data, err := os.ReadFile(wantSidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var fc struct {
		Type     string `json:"type"`
		Features []struct {
			Properties map[string]any `json:"properties"`
			Geometry   struct {
				Type string `json:"type"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v\n%s", err, data)
	}
	if fc.Type != "FeatureCollection" || len(fc.Features) != 1 {
		t.Fatalf("sidecar = %#v", fc)
	}
	if fc.Features[0].Properties["postcode"] != "12345" {
		t.Errorf("postcode property = %v, want 12345", fc.Features[0].Properties["postcode"])
	}
	if fc.Features[0].Geometry.Type != "MultiPolygon" {
		t.Errorf("geometry type = %q, want MultiPolygon", fc.Features[0].Geometry.Type)
	}
}

func TestBuildPBFWithoutPostalCodesWritesNoSidecar(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildPostalFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")
	result, err := BuildPBF(t.Context(), PBFBuildOptions{
		PBFInputs:      []string{pbf},
		ArtifactPath:   artifact,
		MinZoom:        14,
		MaxZoom:        14,
		Concurrency:    1,
		BatchSize:      1,
		MaxMemoryBytes: 64 << 20,
		MinFreeBytes:   1,
	})
	if err != nil {
		t.Fatalf("BuildPBF: %v", err)
	}
	if result.PostalCodeCount != 0 || result.PostalCodesPath != "" {
		t.Fatalf("expected no postal codes when disabled, got %#v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "region.postcodes.geojson")); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar file to be written, stat err = %v", err)
	}
}
