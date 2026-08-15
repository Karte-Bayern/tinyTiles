//go:build !js && !wasm && !baremetal

package tinytiles

import (
	"path/filepath"
	"testing"
)

func TestResolvePresetKnownValues(t *testing.T) {
	tests := []struct {
		preset           Preset
		minZoom, maxZoom int
		tolerance        float64
		postalCodes      bool
	}{
		{PresetBalanced, DefaultPBFBuildMinZoom, DefaultPBFBuildMaxZoom, 4.0, false},
		{PresetFast, 5, 10, 8.0, false},
		{PresetDetailed, 5, 16, 2.0, true},
		{PresetMobile, 5, 12, 6.0, false},
		{PresetPostcode, 5, 13, 4.0, true},
	}
	for _, test := range tests {
		minZoom, maxZoom, tolerance, postalCodes, err := ResolvePreset(test.preset)
		if err != nil {
			t.Fatalf("ResolvePreset(%q): %v", test.preset, err)
		}
		if minZoom != test.minZoom || maxZoom != test.maxZoom || tolerance != test.tolerance || postalCodes != test.postalCodes {
			t.Fatalf("ResolvePreset(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
				test.preset, minZoom, maxZoom, tolerance, postalCodes, test.minZoom, test.maxZoom, test.tolerance, test.postalCodes)
		}
	}
}

func TestResolvePresetEmptyMatchesBalanced(t *testing.T) {
	emptyMinZoom, emptyMaxZoom, emptyTolerance, emptyPostalCodes, err := ResolvePreset("")
	if err != nil {
		t.Fatalf("ResolvePreset(\"\"): %v", err)
	}
	balancedMinZoom, balancedMaxZoom, balancedTolerance, balancedPostalCodes, err := ResolvePreset(PresetBalanced)
	if err != nil {
		t.Fatalf("ResolvePreset(PresetBalanced): %v", err)
	}
	if emptyMinZoom != balancedMinZoom || emptyMaxZoom != balancedMaxZoom || emptyTolerance != balancedTolerance || emptyPostalCodes != balancedPostalCodes {
		t.Fatalf("ResolvePreset(\"\") = (%d,%d,%v,%v), want it to match PresetBalanced (%d,%d,%v,%v)",
			emptyMinZoom, emptyMaxZoom, emptyTolerance, emptyPostalCodes, balancedMinZoom, balancedMaxZoom, balancedTolerance, balancedPostalCodes)
	}
}

func TestResolvePresetUnknownReturnsError(t *testing.T) {
	if _, _, _, _, err := ResolvePreset("bogus"); err == nil {
		t.Fatal("ResolvePreset(\"bogus\") = nil error, want an error")
	}
}

func TestPresetsListsEveryDefinedPreset(t *testing.T) {
	presets := Presets()
	if len(presets) != 5 {
		t.Fatalf("Presets() = %v, want 5 entries", presets)
	}
	for _, p := range presets {
		if _, _, _, _, err := ResolvePreset(p); err != nil {
			t.Fatalf("ResolvePreset(%q) from Presets(): %v", p, err)
		}
	}
}

// TestResolvePBFBuildOptionsAppliesPresetToUnsetFields proves
// resolvePBFBuildOptions fills MinZoom/MaxZoom/SimplifyTolerance from Preset
// when the caller left them at their zero value.
func TestResolvePBFBuildOptionsAppliesPresetToUnsetFields(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildFixture(t, dir, "region.osm.pbf")
	resolved, err := resolvePBFBuildOptions(PBFBuildOptions{
		PBFInputs:    []string{pbf},
		ArtifactPath: filepath.Join(dir, "region.ttiles"),
		Preset:       PresetMobile,
	})
	if err != nil {
		t.Fatalf("resolvePBFBuildOptions: %v", err)
	}
	wantMinZoom, wantMaxZoom, wantTolerance, _, err := ResolvePreset(PresetMobile)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MinZoom != wantMinZoom || resolved.MaxZoom != wantMaxZoom || resolved.SimplifyTolerance != wantTolerance {
		t.Fatalf("resolved = (minZoom=%d, maxZoom=%d, tolerance=%v), want (%d, %d, %v)",
			resolved.MinZoom, resolved.MaxZoom, resolved.SimplifyTolerance, wantMinZoom, wantMaxZoom, wantTolerance)
	}
}

// TestResolvePBFBuildOptionsExplicitFieldsOverridePreset proves an explicit
// MinZoom/MaxZoom/SimplifyTolerance always wins over PresetMobile's values.
func TestResolvePBFBuildOptionsExplicitFieldsOverridePreset(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildFixture(t, dir, "region.osm.pbf")
	resolved, err := resolvePBFBuildOptions(PBFBuildOptions{
		PBFInputs:         []string{pbf},
		ArtifactPath:      filepath.Join(dir, "region.ttiles"),
		Preset:            PresetMobile,
		MinZoom:           7,
		MaxZoom:           9,
		SimplifyTolerance: 1.5,
	})
	if err != nil {
		t.Fatalf("resolvePBFBuildOptions: %v", err)
	}
	if resolved.MinZoom != 7 || resolved.MaxZoom != 9 || resolved.SimplifyTolerance != 1.5 {
		t.Fatalf("resolved = (minZoom=%d, maxZoom=%d, tolerance=%v), want (7, 9, 1.5) — explicit fields must override PresetMobile",
			resolved.MinZoom, resolved.MaxZoom, resolved.SimplifyTolerance)
	}
}

// TestResolvePBFBuildOptionsRejectsUnknownPreset proves an unrecognized
// Preset surfaces as a resolution error rather than silently falling back to
// balanced defaults.
func TestResolvePBFBuildOptionsRejectsUnknownPreset(t *testing.T) {
	dir := t.TempDir()
	pbf := writePBFBuildFixture(t, dir, "region.osm.pbf")
	_, err := resolvePBFBuildOptions(PBFBuildOptions{
		PBFInputs:    []string{pbf},
		ArtifactPath: filepath.Join(dir, "region.ttiles"),
		Preset:       "bogus",
	})
	if err == nil {
		t.Fatal("resolvePBFBuildOptions with Preset \"bogus\": want an error, got nil")
	}
}
