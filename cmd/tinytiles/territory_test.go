//go:build !js && !wasm && !baremetal

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const territoryTestGeoJSON = `{
  "type": "FeatureCollection",
  "features": [
    {"type": "Feature", "properties": {"postcode": "84130"}, "geometry": {"type": "Polygon", "coordinates": [[[0,0],[1,0],[1,1],[0,1],[0,0]]]}},
    {"type": "Feature", "properties": {"postcode": "84131"}, "geometry": {"type": "Polygon", "coordinates": [[[1,0],[2,0],[2,1],[1,1],[1,0]]]}},
    {"type": "Feature", "properties": {"postcode": "94405"}, "geometry": {"type": "Polygon", "coordinates": [[[20,20],[21,20],[21,21],[20,21],[20,20]]]}}
  ]
}`

const territoryTestCSV = `postcode,territory,employee,vehicle
84130,North,Huber,VAN-01
84131,North,Huber,VAN-01
99999,South,Mueller,VAN-02
`

func TestTerritoryBuildPrefixMode(t *testing.T) {
	dir := t.TempDir()
	input := writeTestFile(t, dir, "postcodes.geojson", territoryTestGeoJSON)
	output := filepath.Join(dir, "plz2.geojson")

	var stdout, stderr bytes.Buffer
	code := run([]string{"territory", "--input", input, "--field", "postcode", "--group", "prefix:2", "--output", output}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, data)
	}
	if len(fc.Features) != 2 { // "84" and "94"
		t.Fatalf("expected 2 territories, got %d: %s", len(fc.Features), data)
	}
}

func TestTerritoryBuildMappingModeAndDiagnostics(t *testing.T) {
	dir := t.TempDir()
	input := writeTestFile(t, dir, "postcodes.geojson", territoryTestGeoJSON)
	mapping := writeTestFile(t, dir, "territories.csv", territoryTestCSV)
	output := filepath.Join(dir, "territories.geojson")

	var stdout, stderr bytes.Buffer
	code := run([]string{"territory", "--input", input, "--mapping", mapping, "--group-by", "territory", "--output", output}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("unmatched-geometries=1")) {
		t.Errorf("expected the unmatched 94405 geometry to be reported, got stdout=%q", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("unmatched-mapping-rows=1")) {
		t.Errorf("expected the unmatched 99999 mapping row to be reported, got stdout=%q", stdout.String())
	}

	// Rerunning without --replace must refuse to overwrite.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"territory", "--input", input, "--mapping", mapping, "--group-by", "territory", "--output", output}, &stdout, &stderr)
	if code != 2 || !bytes.Contains(stderr.Bytes(), []byte("already exists")) {
		t.Fatalf("expected a refusal to overwrite, got code=%d stderr=%q", code, stderr.String())
	}
}

func TestTerritoryValidateMappingOnly(t *testing.T) {
	dir := t.TempDir()
	mapping := writeTestFile(t, dir, "territories.csv", territoryTestCSV)

	var stdout, stderr bytes.Buffer
	code := run([]string{"territory", "validate", mapping}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("validate output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if report["ok"] != true {
		t.Errorf("expected ok=true for a mapping file with no duplicate keys, got %v", report)
	}
}

func TestTerritoryInspect(t *testing.T) {
	dir := t.TempDir()
	input := writeTestFile(t, dir, "postcodes.geojson", territoryTestGeoJSON)

	var stdout, stderr bytes.Buffer
	code := run([]string{"territory", "inspect", input}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report struct {
		FeatureCount int `json:"FeatureCount"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("inspect output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if report.FeatureCount != 3 {
		t.Errorf("FeatureCount = %d, want 3", report.FeatureCount)
	}
}

func TestTerritoryBuildRejectsConflictingModes(t *testing.T) {
	dir := t.TempDir()
	input := writeTestFile(t, dir, "postcodes.geojson", territoryTestGeoJSON)
	mapping := writeTestFile(t, dir, "territories.csv", territoryTestCSV)
	output := filepath.Join(dir, "out.geojson")

	var stdout, stderr bytes.Buffer
	code := run([]string{"territory", "--input", input, "--field", "postcode", "--group", "prefix:2", "--mapping", mapping, "--group-by", "territory", "--output", output}, &stdout, &stderr)
	if code != 2 || !bytes.Contains(stderr.Bytes(), []byte("mutually exclusive")) {
		t.Fatalf("expected a mutually-exclusive-modes error, got code=%d stderr=%q", code, stderr.String())
	}
}
