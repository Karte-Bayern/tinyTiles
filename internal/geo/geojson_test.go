package geo

import (
	"encoding/json"
	"testing"
)

func TestGeoJSONRoundTrip(t *testing.T) {
	mp := MultiPolygon{
		{Rings: []Ring{square(0, 0, 1, 1)}},
		{Rings: []Ring{square(10, 10, 11, 11), square(10.2, 10.2, 10.8, 10.8)}},
	}
	features := []Feature{{Properties: map[string]any{"territory": "North", "count": 2.0}, Geometry: mp}}

	data, err := WriteFeatureCollection(features)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadFeatures(data)
	if err != nil {
		t.Fatalf("ReadFeatures: %v\n%s", err, data)
	}
	if len(back) != 1 {
		t.Fatalf("expected 1 feature back, got %d", len(back))
	}
	if back[0].Properties["territory"] != "North" {
		t.Errorf("properties = %#v", back[0].Properties)
	}
	if len(back[0].Geometry) != 2 || len(back[0].Geometry[1].Rings) != 2 {
		t.Fatalf("geometry round-trip mismatch: %#v", back[0].Geometry)
	}
}

func TestReadFeaturesRejectsNonPolygonGeometry(t *testing.T) {
	data := []byte(`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{},"geometry":{"type":"Point","coordinates":[0,0]}}]}`)
	if _, err := ReadFeatures(data); err == nil {
		t.Fatal("expected an error for a Point geometry")
	}
}

func TestWriteFeatureCollectionIsDeterministic(t *testing.T) {
	features := []Feature{{
		Properties: map[string]any{"z": 1, "a": 2, "m": 3},
		Geometry:   MultiPolygon{{Rings: []Ring{square(0, 0, 1, 1)}}},
	}}
	first, err := WriteFeatureCollection(features)
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteFeatureCollection(features)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("WriteFeatureCollection produced different output for identical input")
	}
	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}
