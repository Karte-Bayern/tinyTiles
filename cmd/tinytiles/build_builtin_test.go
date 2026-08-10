//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// writeBuiltinPBFFixture writes a minimal PBF with one tagged, renderable
// way (a two-node residential street) so the built-in generator's ordinary
// bounds/feature check passes. It is the CLI-package-local twin of the root
// package's writePBFBuildFixture: `cmd/tinytiles` cannot import that
// unexported test helper across packages.
func writeBuiltinPBFFixture(t *testing.T, dir, name string) string {
	t.Helper()
	stringsTable := message(bytesField(1, []byte{}), bytesField(1, []byte("highway")), bytesField(1, []byte("residential")))
	dense := message(
		bytesField(1, packed(zigzag(10), zigzag(1))),
		bytesField(8, packed(zigzag(500_000_000), zigzag(10))),
		bytesField(9, packed(zigzag(80_000_000), zigzag(10))),
	)
	way := message(
		varintField(1, 1),
		bytesField(2, packed(1)),
		bytesField(3, packed(2)),
		bytesField(8, packed(zigzag(10), zigzag(1))),
	)
	block := message(bytesField(1, stringsTable), bytesField(2, message(bytesField(2, dense), bytesField(3, way))))
	blob := message(bytesField(1, block))
	header := message(bytesField(1, []byte("OSMData")), varintField(3, uint64(len(blob))))
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

func message(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
func bytesField(field int, value []byte) []byte {
	out := appendVarintBytes(nil, uint64(field<<3|2))
	out = appendVarintBytes(out, uint64(len(value)))
	return append(out, value...)
}
func varintField(field int, value uint64) []byte {
	return appendVarintBytes(appendVarintBytes(nil, uint64(field<<3)), value)
}
func packed(values ...uint64) []byte {
	var out []byte
	for _, v := range values {
		out = appendVarintBytes(out, v)
	}
	return out
}
func appendVarintBytes(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}
func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// TestCommandBuildBuiltinGeneratorEndToEnd exercises `tinytiles build`
// through the CLI entrypoint with no --generator (the built-in generator,
// and the exact invocation the README's Quick start documents). It
// previously failed unconditionally: the built-in generator's tile-stream
// output was passed to the SQLite MBTiles importer, which rejected it as
// "not a database".
func TestCommandBuildBuiltinGeneratorEndToEnd(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--minzoom", "14", "--maxzoom", "14", "--max-memory", "67108864", "--min-free", "1", pbf, artifact}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "generator=tinytiles-minimal") || !strings.Contains(stdout.String(), "artifact="+artifact) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if _, err := tiles.ValidateArtifact(t.Context(), artifact); err != nil {
		t.Fatalf("validate published artifact: %v", err)
	}
}

func TestCommandBuildRejectsCompactWithBuiltinGenerator(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--compact", pbf, artifact}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--compact requires an external --generator") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestCommandBuildRejectsMbtilesOutWithBuiltinGenerator(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--mbtiles-out", filepath.Join(dir, "out.mbtiles"), pbf, artifact}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--mbtiles-out requires an external --generator") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

// writeBuiltinPostalPBFFixture adds one boundary=postal_code relation over a
// second, building-tagged way to writeBuiltinPBFFixture's highway feature.
func writeBuiltinPostalPBFFixture(t *testing.T, dir, name string) string {
	t.Helper()
	// table: 0 unused, 1 highway, 2 residential, 3 boundary, 4 postal_code,
	// 5 12345, 6 outer, 7 building, 8 yes
	stringsTable := message(
		bytesField(1, []byte{}),
		bytesField(1, []byte("highway")),
		bytesField(1, []byte("residential")),
		bytesField(1, []byte("boundary")),
		bytesField(1, []byte("postal_code")),
		bytesField(1, []byte("12345")),
		bytesField(1, []byte("outer")),
		bytesField(1, []byte("building")),
		bytesField(1, []byte("yes")),
	)
	dense := message(
		bytesField(1, packed(zigzag(10), zigzag(1), zigzag(9), zigzag(1), zigzag(1), zigzag(1))),
		bytesField(8, packed(zigzag(500_000_000), zigzag(10), zigzag(0), zigzag(0), zigzag(100_000), zigzag(0))),
		bytesField(9, packed(zigzag(80_000_000), zigzag(10), zigzag(0), zigzag(100_000), zigzag(0), zigzag(-100_000))),
	)
	highwayWay := message(varintField(1, 1), bytesField(2, packed(1)), bytesField(3, packed(2)), bytesField(8, packed(zigzag(10), zigzag(1))))
	postalWay := message(
		varintField(1, 100),
		bytesField(2, packed(7)),
		bytesField(3, packed(8)),
		bytesField(8, packed(zigzag(20), zigzag(1), zigzag(1), zigzag(1), zigzag(-3))),
	)
	relation := message(
		varintField(1, 500),
		bytesField(2, packed(3, 4)),
		bytesField(3, packed(4, 5)),
		bytesField(8, packed(6)),
		bytesField(9, packed(zigzag(100))),
		bytesField(10, packed(1)),
	)
	block := message(bytesField(1, stringsTable), bytesField(2, message(
		bytesField(2, dense), bytesField(3, highwayWay), bytesField(3, postalWay), bytesField(4, relation),
	)))
	blob := message(bytesField(1, block))
	header := message(bytesField(1, []byte("OSMData")), varintField(3, uint64(len(blob))))
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

func TestCommandBuildBuiltinGeneratorWithPostalCodes(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPostalPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--postal-codes", "--minzoom", "14", "--maxzoom", "14", pbf, artifact}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	sidecar := filepath.Join(dir, "region.postcodes.geojson")
	if !strings.Contains(stdout.String(), "postal-codes=1") || !strings.Contains(stdout.String(), sidecar) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !bytes.Contains(data, []byte(`"postcode": "12345"`)) {
		t.Fatalf("sidecar missing postcode property: %s", data)
	}
}
