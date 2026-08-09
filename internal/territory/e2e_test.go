package territory

import (
	"encoding/json"
	"testing"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

func sq(x0, y0, x1, y1 float64) geo.Ring {
	return geo.Ring{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}
}

func feature(postcode string, rings ...geo.Ring) geo.Feature {
	return geo.Feature{
		Properties: map[string]any{"postcode": postcode},
		Geometry:   geo.MultiPolygon{{Rings: rings}},
	}
}

// fixture is a small synthetic "postcode" dataset exercising every grouping
// scenario below: 84130/84131 touch (merge on dissolve), 84307/84323 touch
// while 84399 sits far away in the same prefix:3 group (disconnected
// territory), 77000 carries a hole, and several postcodes are deliberately
// left out of the mapping CSV (or vice versa) to exercise unmatched joins.
func fixture() []geo.Feature {
	return []geo.Feature{
		feature("84130", sq(0, 0, 1, 1)),
		feature("84131", sq(1, 0, 2, 1)),
		feature("84140", sq(0, 2, 1, 3)),
		feature("84230", sq(5, 5, 6, 6)),
		feature("84307", sq(10, 10, 11, 11)),
		feature("84323", sq(11, 10, 12, 11)),
		feature("84399", sq(50, 50, 51, 51)),
		feature("94405", sq(20, 20, 21, 21)),
		feature("77000", sq(30, 30, 34, 34), sq(31, 31, 33, 33)),
	}
}

const mappingCSV = `postcode,territory,employee,vehicle
84130,North,Huber,VAN-01
84131,North,Huber,VAN-01
84230,North,Meier,VAN-01
84307,South,Mueller,VAN-02
84323,South,Mueller,VAN-02
99999,South,Mueller,VAN-02
84130,Other,Foo,VAN-99
`

// 1. Grouping by first digit.
func TestPrefixGroupingByOneDigit(t *testing.T) {
	result, err := Build(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := territoryIDs(result.Territories)
	want := []string{"7", "8", "9"}
	assertIDs(t, got, want)
	byID := index(result.Territories)
	if byID["8"].Properties["source_feature_count"] != 7 {
		t.Errorf(`territory "8" source_feature_count = %v, want 7`, byID["8"].Properties["source_feature_count"])
	}
}

// 2. Grouping by first two digits.
func TestPrefixGroupingByTwoDigits(t *testing.T) {
	result, err := Build(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, territoryIDs(result.Territories), []string{"77", "84", "94"})
}

// 3. Grouping by first three digits, including the disconnected-territory
// and touching-boundary-merge cases (scenario 5).
func TestPrefixGroupingByThreeDigits(t *testing.T) {
	result, err := Build(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 3})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, territoryIDs(result.Territories), []string{"770", "841", "842", "843", "944"})

	byID := index(result.Territories)
	t841 := byID["841"]
	if n := t841.Properties["source_feature_count"]; n != 3 { // 84130, 84131, 84140
		t.Errorf(`"841" source_feature_count = %v, want 3`, n)
	}
	if len(t841.Geometry) != 2 { // 84130+84131 merge (touching); 84140 stands alone
		t.Errorf(`"841" should have 2 components (one merged pair + one standalone), got %d`, len(t841.Geometry))
	}

	// 5. Disconnected territories: 84307+84323 touch and merge into one
	// component, 84399 sits far away as a second component.
	t843 := byID["843"]
	if len(t843.Geometry) != 2 {
		t.Fatalf(`"843" should have 2 disconnected components, got %d: %#v`, len(t843.Geometry), t843.Geometry)
	}
}

// 6. Holes in polygons survive the full build pipeline.
func TestHolesSurviveBuild(t *testing.T) {
	result, err := Build(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 1})
	if err != nil {
		t.Fatal(err)
	}
	t7 := index(result.Territories)["7"]
	if len(t7.Geometry) != 1 || len(t7.Geometry[0].Rings) != 2 {
		t.Fatalf("expected one polygon with a hole, got %#v", t7.Geometry)
	}
	outerArea := geo.AreaKM2(geo.MultiPolygon{{Rings: []geo.Ring{t7.Geometry[0].Rings[0]}}})
	fullArea := t7.Properties["area_km2"].(float64)
	if fullArea >= outerArea {
		t.Errorf("area_km2 = %v should be less than the outer ring alone (%v); hole not subtracted", fullArea, outerArea)
	}
}

func csvMapping(t *testing.T) *MappingTable {
	t.Helper()
	table, err := parseCSVMapping([]byte(mappingCSV))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// 4. Arbitrary CSV mapping grouped by an arbitrary column ("territory"),
// including default aggregation behavior when source rows disagree.
func TestArbitraryCSVMapping(t *testing.T) {
	result, err := Build(Options{
		Features:    fixture(),
		GeometryKey: "postcode",
		Mapping:     csvMapping(t),
		MappingKey:  "postcode",
		GroupBy:     "territory",
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := index(result.Territories)
	assertIDs(t, territoryIDs(result.Territories), []string{"North", "Other", "South"})

	north := byID["North"]
	if n := north.Properties["source_feature_count"]; n != 3 {
		t.Fatalf("North source_feature_count = %v, want 3", n)
	}
	// employee disagrees (Huber vs Meier) -> unique list; vehicle agrees -> scalar.
	employees, ok := north.Properties["employee"].([]any)
	if !ok || len(employees) != 2 {
		t.Fatalf("North employee = %#v, want a 2-element list", north.Properties["employee"])
	}
	if north.Properties["vehicle"] != "VAN-01" {
		t.Errorf("North vehicle = %#v, want scalar VAN-01", north.Properties["vehicle"])
	}

	south := byID["South"]
	if len(south.Geometry) != 1 { // 84307+84323 touch and merge
		t.Errorf("South should merge into 1 component, got %d", len(south.Geometry))
	}
}

// 7. Missing mappings in both directions.
func TestMissingMappingsBothDirections(t *testing.T) {
	result, err := Build(Options{
		Features:    fixture(),
		GeometryKey: "postcode",
		Mapping:     csvMapping(t),
		MappingKey:  "postcode",
		GroupBy:     "territory",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, result.Diagnostics.UnmatchedGeometryKeys, []string{"77000", "84140", "84399", "94405"})
	assertIDs(t, result.Diagnostics.UnmatchedMappingKeys, []string{"99999"})
}

// 8. Duplicate mapping keys are surfaced, not silently dropped: 84130
// contributes its geometry to both North (its first row) and Other (its
// second row), and the duplicate is reported.
func TestDuplicateMappingKeys(t *testing.T) {
	result, err := Build(Options{
		Features:    fixture(),
		GeometryKey: "postcode",
		Mapping:     csvMapping(t),
		MappingKey:  "postcode",
		GroupBy:     "territory",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, result.Diagnostics.DuplicateMappingKeys, []string{"84130"})
	other := index(result.Territories)["Other"]
	if other.Properties["source_feature_count"] != 1 {
		t.Fatalf("Other source_feature_count = %v, want 1", other.Properties["source_feature_count"])
	}
}

// 9. Simplification removes redundant collinear vertices left behind by a
// dissolve while leaving the shape (area, bbox) intact.
func TestSimplificationRemovesRedundantVertices(t *testing.T) {
	toleranceMeters, err := ParseDistance("1m")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Build(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 3, SimplifyMeters: toleranceMeters})
	if err != nil {
		t.Fatal(err)
	}
	t843 := index(result.Territories)["843"]
	merged := largestByBBoxWidth(t843.Geometry)
	if len(merged.Rings[0]) != 5 { // 4 distinct points + closing point
		t.Errorf("expected the merged rectangle simplified to 4 corners, got %d points: %v", len(merged.Rings[0])-1, merged.Rings[0])
	}
}

// 10. Deterministic output: rebuilding from identical input produces
// byte-identical GeoJSON.
func TestDeterministicOutput(t *testing.T) {
	build := func() []byte {
		result, err := Build(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 2})
		if err != nil {
			t.Fatal(err)
		}
		data, err := ToGeoJSON(result.Territories)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first, second := build(), build()
	if string(first) != string(second) {
		t.Fatal("two builds from identical input produced different GeoJSON")
	}
}

// 11. Power BI export drops the raw source_values list but keeps stable IDs
// and area.
func TestPowerBIExport(t *testing.T) {
	result, err := Build(Options{
		Features:    fixture(),
		GeometryKey: "postcode",
		Mapping:     csvMapping(t),
		MappingKey:  "postcode",
		GroupBy:     "territory",
	})
	if err != nil {
		t.Fatal(err)
	}
	trimmed := PowerBIPreset(result.Territories)
	data, err := ToGeoJSON(trimmed)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, f := range decoded.Features {
		if _, present := f.Properties["source_values"]; present {
			t.Errorf("powerbi preset should drop source_values, found it in %#v", f.Properties)
		}
		if _, present := f.Properties["territory_id"]; !present {
			t.Error("powerbi preset must keep territory_id")
		}
		if _, present := f.Properties["area_km2"]; !present {
			t.Error("powerbi preset must keep area_km2")
		}
	}
}

func territoryIDs(territories []Territory) []string {
	out := make([]string, len(territories))
	for i, t := range territories {
		out[i] = t.ID
	}
	return out
}

func index(territories []Territory) map[string]Territory {
	out := make(map[string]Territory, len(territories))
	for _, t := range territories {
		out[t.ID] = t
	}
	return out
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func largestByBBoxWidth(mp geo.MultiPolygon) geo.Polygon {
	best := mp[0]
	bestWidth := -1.0
	for _, poly := range mp {
		bbox, ok := geo.BBox(geo.MultiPolygon{poly})
		if !ok {
			continue
		}
		width := bbox[2] - bbox[0]
		if width > bestWidth {
			best, bestWidth = poly, width
		}
	}
	return best
}
