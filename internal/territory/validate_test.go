package territory

import (
	"testing"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

func TestValidateReportsIssues(t *testing.T) {
	report, err := Validate(Options{
		Features:    fixture(),
		GeometryKey: "postcode",
		Mapping:     csvMapping(t),
		MappingKey:  "postcode",
		GroupBy:     "territory",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK {
		t.Fatal("expected OK=false given unmatched and duplicate mapping keys")
	}
	assertIDs(t, report.UnmatchedMappingKeys, []string{"99999"})
	assertIDs(t, report.DuplicateMappingKeys, []string{"84130"})
	if len(report.UnmatchedGeometryKeys) != 4 {
		t.Errorf("UnmatchedGeometryKeys = %v", report.UnmatchedGeometryKeys)
	}
}

func TestValidateOKOnCleanInput(t *testing.T) {
	report, err := Validate(Options{Features: fixture(), GeometryKey: "postcode", PrefixLength: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("expected a clean prefix-grouped build to validate OK, got %#v", report)
	}
}

func TestDetectOverlapsFlagsOverlappingPolygons(t *testing.T) {
	overlapping := []geo.Feature{
		{Properties: map[string]any{"postcode": "A"}, Geometry: geo.MultiPolygon{{Rings: []geo.Ring{sq(0, 0, 2, 2)}}}},
		{Properties: map[string]any{"postcode": "B"}, Geometry: geo.MultiPolygon{{Rings: []geo.Ring{sq(1, 1, 3, 3)}}}},
	}
	report, err := Validate(Options{Features: overlapping, GeometryKey: "postcode", PrefixLength: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.PossibleOverlaps) != 1 {
		t.Fatalf("expected one overlap warning, got %v", report.PossibleOverlaps)
	}
}

func TestInspectSummarizesFeatures(t *testing.T) {
	report := Inspect(fixture())
	if report.FeatureCount != len(fixture()) {
		t.Fatalf("FeatureCount = %d, want %d", report.FeatureCount, len(fixture()))
	}
	if !report.HasBBox {
		t.Fatal("expected a bbox for a non-empty feature set")
	}
	if report.TotalAreaKM2 <= 0 {
		t.Errorf("TotalAreaKM2 = %v, want > 0", report.TotalAreaKM2)
	}
}
