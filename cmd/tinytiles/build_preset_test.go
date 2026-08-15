//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandBuildPresetAppliesZoomRange builds writeBuiltinPBFFixture's
// residential way (minZoom 11) under --preset fast (zoom 5-10): the way is
// classified but never visible in the requested range, so the build must
// fail with "no tiles" rather than silently using the balanced 5-14 range.
func TestCommandBuildPresetAppliesZoomRange(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--preset", "fast", pbf, artifact}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "no tiles") {
		t.Fatalf("code=%d stdout=%q stderr=%q, want a preset-fast build (zoom 5-10) to fail on a minZoom-11 way", code, stdout.String(), stderr.String())
	}
}

// TestCommandBuildPresetDefaultRangeRendersWay is the balanced-preset
// counterpart of the above: the same minZoom-11 way must render across the
// balanced zoom range (5-14), i.e. once at each of zooms 11-14.
func TestCommandBuildPresetDefaultRangeRendersWay(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--preset", "balanced", pbf, artifact}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "road-features=4 ") {
		t.Fatalf("stdout=%q, want the way visible at zooms 11-14 (4 zoom levels)", stdout.String())
	}
}

// TestCommandBuildExplicitZoomOverridesPreset proves an explicit --maxzoom
// wins over the preset's maxzoom: --preset fast alone would clip the way at
// zoom 10 (see TestCommandBuildPresetAppliesZoomRange), but --maxzoom 14
// pushes the effective range to 5-14, rendering it at zooms 11-14 same as
// the balanced default.
func TestCommandBuildExplicitZoomOverridesPreset(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--preset", "fast", "--maxzoom", "14", pbf, artifact}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "road-features=4 ") {
		t.Fatalf("stdout=%q, want --maxzoom 14 to override --preset fast's maxzoom 10", stdout.String())
	}
}

// TestCommandBuildExplicitPostalCodesOverridesPreset proves an explicit
// --postal-codes=false wins over a preset that would otherwise enable it.
func TestCommandBuildExplicitPostalCodesOverridesPreset(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPostalPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--preset", "postcode", "--postal-codes=false", "--minzoom", "14", "--maxzoom", "14", pbf, artifact}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "postal-codes=") {
		t.Fatalf("stdout=%q, want --postal-codes=false to override --preset postcode's postal-codes suggestion", stdout.String())
	}
}

func TestCommandBuildRejectsUnknownPreset(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--preset", "bogus", pbf, artifact}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `unknown preset "bogus"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestCommandBuildRejectsPresetWithExternalGenerator(t *testing.T) {
	dir := t.TempDir()
	pbf := writeBuiltinPBFFixture(t, dir, "region.osm.pbf")
	artifact := filepath.Join(dir, "region.ttiles")

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--generator", "does-not-matter", "--preset", "mobile", pbf, artifact}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--preset requires the built-in generator") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
