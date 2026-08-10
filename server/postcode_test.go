//go:build sqliteimport

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func doGetStatus(t *testing.T, server *Server, path string) int {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://tiles.example"+path, nil))
	return response.Code
}

func doGet(t *testing.T, server *Server, path string) map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://tiles.example"+path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d: %s", path, response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: not valid JSON: %v\n%s", path, err, response.Body.String())
	}
	return body
}

const postcodeFixtureGeoJSON = `{
  "type": "FeatureCollection",
  "features": [
    {"type": "Feature", "properties": {"postcode": "84130", "name": "Passau-Nord"}, "geometry": {"type": "MultiPolygon", "coordinates": [[[[12.6,48.6],[12.7,48.6],[12.7,48.68],[12.6,48.68],[12.6,48.6]]]]}},
    {"type": "Feature", "properties": {"postcode": "94405", "name": "Landau"}, "geometry": {"type": "MultiPolygon", "coordinates": [[[[12.95,48.95],[13.05,48.95],[13.05,49.03],[12.95,49.03],[12.95,48.95]]]]}}
  ]
}`

func writePostcodeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "postcodes.geojson")
	if err := os.WriteFile(path, []byte(postcodeFixtureGeoJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPostcodeIndex(t *testing.T) {
	idx, err := loadPostcodeIndex(writePostcodeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.all) != 2 {
		t.Fatalf("records = %d, want 2", len(idx.all))
	}
	rec, ok := idx.byCode["84130"]
	if !ok || rec.Name != "Passau-Nord" {
		t.Fatalf("byCode[84130] = %#v, ok=%v", rec, ok)
	}
	if !rec.HasBBox || rec.Center[0] < 12.6 || rec.Center[0] > 12.7 {
		t.Fatalf("center = %v", rec.Center)
	}
	// Sorted by code.
	if idx.all[0].Code != "84130" || idx.all[1].Code != "94405" {
		t.Fatalf("all = %#v", idx.all)
	}
}

func TestServePostcodeLookup(t *testing.T) {
	server, err := New(Config{Dataset: testDataset(t), DatasetID: "fixture", PostcodeIndexPath: writePostcodeFixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	body := doGet(t, server, "/postcode/84130")
	if body["postcode"] != "84130" || body["name"] != "Passau-Nord" {
		t.Fatalf("lookup = %#v", body)
	}
	if _, ok := body["geometry"]; !ok {
		t.Fatal("expected geometry in a direct lookup")
	}

	notFound := doGetStatus(t, server, "/postcode/00000")
	if notFound != 404 {
		t.Fatalf("status = %d, want 404", notFound)
	}
}

func TestServePostcodeSearch(t *testing.T) {
	server, err := New(Config{Dataset: testDataset(t), DatasetID: "fixture", PostcodeIndexPath: writePostcodeFixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	body := doGet(t, server, "/postcode/search?q=841")
	results, ok := body["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("search results = %#v", body["results"])
	}
	first, ok := results[0].(map[string]any)
	if !ok || first["postcode"] != "84130" {
		t.Fatalf("first result = %#v", results[0])
	}
	if _, hasGeometry := first["geometry"]; hasGeometry {
		t.Error("search results must not include full geometry")
	}

	all := doGet(t, server, "/postcode/search")
	if results, ok := all["results"].([]any); !ok || len(results) != 2 {
		t.Fatalf("empty-query search results = %#v", all["results"])
	}
}

func TestServePostcodeAt(t *testing.T) {
	server, err := New(Config{Dataset: testDataset(t), DatasetID: "fixture", PostcodeIndexPath: writePostcodeFixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	inside := doGet(t, server, "/postcode/at?lon=12.65&lat=48.64")
	results, ok := inside["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("inside results = %#v", inside["results"])
	}
	rec, ok := results[0].(map[string]any)
	if !ok || rec["postcode"] != "84130" {
		t.Fatalf("inside result = %#v", results[0])
	}

	outside := doGet(t, server, "/postcode/at?lon=0&lat=0")
	if results, ok := outside["results"].([]any); !ok || len(results) != 0 {
		t.Fatalf("outside results = %#v", outside["results"])
	}

	badStatus := doGetStatus(t, server, "/postcode/at?lon=notanumber&lat=0")
	if badStatus != 400 {
		t.Fatalf("bad coordinate status = %d, want 400", badStatus)
	}
}

func TestPostcodeRoutesUnregisteredWithoutIndex(t *testing.T) {
	server, err := New(Config{Dataset: testDataset(t), DatasetID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if status := doGetStatus(t, server, "/postcode/84130"); status != 404 {
		t.Fatalf("status = %d, want 404 when no PostcodeIndexPath is configured", status)
	}
}
