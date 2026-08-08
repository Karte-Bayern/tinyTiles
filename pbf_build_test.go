//go:build !js && !wasm && !baremetal

package tinytiles

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestBuildPBFPublishesValidatedArtifact(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")
	var phases []string
	result, err := BuildPBF(t.Context(), PBFBuildOptions{
		PBFInputs:      []string{pbf},
		ArtifactPath:   artifact,
		MinZoom:        14,
		MaxZoom:        14,
		Concurrency:    1,
		BatchSize:      1,
		MaxMemoryBytes: 64 << 20,
		MinFreeBytes:   1,
		Progress:       func(progress PBFBuildProgress) { phases = append(phases, progress.Phase) },
	})
	if err != nil {
		t.Fatalf("BuildPBF: %v", err)
	}
	if result.ArtifactPath != artifact {
		t.Fatalf("artifact path = %q, want %q", result.ArtifactPath, artifact)
	}
	if result.GeneratedTiles == 0 || result.RoadFeatures == 0 {
		t.Fatalf("generation result = %#v, want tiles and road features", result)
	}
	if result.Bounds.MinLon >= result.Bounds.MaxLon || result.Bounds.MinLat >= result.Bounds.MaxLat {
		t.Fatalf("invalid generation bounds: %#v", result.Bounds)
	}
	if result.Info.Schema != tiles.SchemaFlat {
		t.Fatalf("schema = %q, want flat", result.Info.Schema)
	}
	if _, err := tiles.ValidateArtifact(t.Context(), artifact); err != nil {
		t.Fatalf("validate published artifact: %v", err)
	}
	if !containsPBFBuildPhase(phases, "generate") || !containsPBFBuildPhase(phases, "generated") || !containsPBFBuildPhase(phases, "preflight") || !containsPBFBuildPhase(phases, "published") {
		t.Fatalf("progress phases = %q, want generation and import lifecycle", phases)
	}
	generator, ok := result.Info.Provenance["generator"].(map[string]any)
	if !ok || generator["adapter"] != "tinytiles-minimal" || generator["executable"] != "builtin" {
		t.Fatalf("generator provenance = %#v", result.Info.Provenance["generator"])
	}
	config, ok := result.Info.Provenance["generator_config"].(map[string]any)
	if !ok {
		t.Fatalf("generator config provenance = %#v", result.Info.Provenance["generator_config"])
	}
	layers, ok := config["layers"].([]any)
	if !ok || len(layers) != 4 || layers[0] != "water" || layers[3] != "transportation" {
		t.Fatalf("layer provenance = %#v", config["layers"])
	}
	assertPBFBuildWorkspaceClean(t, dir)
}

func TestBuildPBFPreservesExistingArtifactWhenGenerationFails(t *testing.T) {
	dir := t.TempDir()
	goodPBF := writePBFBuildFixture(t, dir, "good.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")
	first, err := BuildPBF(t.Context(), PBFBuildOptions{
		PBFInputs:      []string{goodPBF},
		ArtifactPath:   artifact,
		MinZoom:        14,
		MaxZoom:        14,
		Concurrency:    1,
		MaxMemoryBytes: 64 << 20,
		MinFreeBytes:   1,
	})
	if err != nil {
		t.Fatalf("initial BuildPBF: %v", err)
	}
	broken := filepath.Join(dir, "broken.osm.pbf")
	if err := os.WriteFile(broken, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = BuildPBF(t.Context(), PBFBuildOptions{
		PBFInputs:       []string{broken},
		ArtifactPath:    artifact,
		MinZoom:         14,
		MaxZoom:         14,
		Concurrency:     1,
		MaxMemoryBytes:  64 << 20,
		MinFreeBytes:    1,
		ReplaceExisting: true,
	})
	if err == nil || !strings.Contains(err.Error(), "generate PBF tiles") {
		t.Fatalf("broken PBF error = %v, want generation error", err)
	}
	after, err := tiles.ValidateArtifact(t.Context(), artifact)
	if err != nil {
		t.Fatalf("validate preserved artifact: %v", err)
	}
	if after.TileDigestSHA256 != first.Info.TileDigestSHA256 {
		t.Fatalf("failed rebuild changed existing artifact digest: got %s want %s", after.TileDigestSHA256, first.Info.TileDigestSHA256)
	}
	assertPBFBuildWorkspaceClean(t, dir)
	if _, err := os.Stat(artifact + ".rollback"); !os.IsNotExist(err) {
		t.Fatalf("unexpected rollback artifact after failed generation: %v", err)
	}
}

func TestBuildPBFRejectsArtifactThatWouldReplaceInput(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildFixture(t, dir, "region.osm.pbf")
	_, err := BuildPBF(t.Context(), PBFBuildOptions{
		PBFInputs:       []string{pbf},
		ArtifactPath:    pbf,
		MaxMemoryBytes:  64 << 20,
		MinFreeBytes:    1,
		ReplaceExisting: true,
	})
	if err == nil || !strings.Contains(err.Error(), "must not replace PBF input") {
		t.Fatalf("overlapping artifact error = %v", err)
	}
}

func containsPBFBuildPhase(phases []string, phase string) bool {
	for _, candidate := range phases {
		if candidate == phase {
			return true
		}
	}
	return false
}

func assertPBFBuildWorkspaceClean(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tinytiles-pbf-") {
			t.Fatalf("temporary PBF workspace was not removed: %s", entry.Name())
		}
	}
}

func writePBFBuildFixture(t *testing.T, dir, name string) string {
	t.Helper()
	stringsTable := pbfBuildMessage(
		pbfBuildBytesField(1, []byte{}),
		pbfBuildBytesField(1, []byte("highway")),
		pbfBuildBytesField(1, []byte("residential")),
	)
	dense := pbfBuildMessage(
		pbfBuildBytesField(1, pbfBuildPacked(pbfBuildZigZag(10), pbfBuildZigZag(1))),
		pbfBuildBytesField(8, pbfBuildPacked(pbfBuildZigZag(500_000_000), pbfBuildZigZag(10))),
		pbfBuildBytesField(9, pbfBuildPacked(pbfBuildZigZag(80_000_000), pbfBuildZigZag(10))),
	)
	way := pbfBuildMessage(
		pbfBuildBytesField(2, pbfBuildPacked(1)),
		pbfBuildBytesField(3, pbfBuildPacked(2)),
		pbfBuildBytesField(8, pbfBuildPacked(pbfBuildZigZag(10), pbfBuildZigZag(1))),
	)
	block := pbfBuildMessage(
		pbfBuildBytesField(1, stringsTable),
		pbfBuildBytesField(2, pbfBuildMessage(pbfBuildBytesField(2, dense), pbfBuildBytesField(3, way))),
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

func pbfBuildMessage(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func pbfBuildBytesField(field int, value []byte) []byte {
	out := pbfBuildAppendVarint(nil, uint64(field<<3|2))
	out = pbfBuildAppendVarint(out, uint64(len(value)))
	return append(out, value...)
}

func pbfBuildVarintField(field int, value uint64) []byte {
	out := pbfBuildAppendVarint(nil, uint64(field<<3))
	return pbfBuildAppendVarint(out, value)
}

func pbfBuildPacked(values ...uint64) []byte {
	var out []byte
	for _, value := range values {
		out = pbfBuildAppendVarint(out, value)
	}
	return out
}

func pbfBuildAppendVarint(out []byte, value uint64) []byte {
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func pbfBuildZigZag(value int64) uint64 { return uint64(value<<1) ^ uint64(value>>63) }
