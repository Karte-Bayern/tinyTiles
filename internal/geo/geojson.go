package geo

import (
	"encoding/json"
	"fmt"
	"math"
)

// Feature pairs a MultiPolygon with arbitrary GeoJSON properties.
type Feature struct {
	Properties map[string]any
	Geometry   MultiPolygon
}

type rawFeatureCollection struct {
	Type     string       `json:"type"`
	Features []rawFeature `json:"features"`
}

type rawFeature struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
	Geometry   rawGeometry     `json:"geometry"`
}

type rawGeometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

// ReadFeatures parses a GeoJSON FeatureCollection (or a single bare Feature)
// whose geometries are Polygon or MultiPolygon. Any other top-level or
// geometry type is reported as an error naming the offending feature index —
// a territory input has no meaningful non-areal member.
func ReadFeatures(data []byte) ([]Feature, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("geo: parse GeoJSON: %w", err)
	}
	var rawFeatures []rawFeature
	switch probe.Type {
	case "FeatureCollection":
		var fc rawFeatureCollection
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, fmt.Errorf("geo: parse FeatureCollection: %w", err)
		}
		rawFeatures = fc.Features
	case "Feature":
		var f rawFeature
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("geo: parse Feature: %w", err)
		}
		rawFeatures = []rawFeature{f}
	default:
		return nil, fmt.Errorf("geo: unsupported top-level GeoJSON type %q", probe.Type)
	}

	features := make([]Feature, 0, len(rawFeatures))
	for i, rf := range rawFeatures {
		geometry, err := decodeGeometry(rf.Geometry)
		if err != nil {
			return nil, fmt.Errorf("geo: feature %d: %w", i, err)
		}
		var props map[string]any
		if len(rf.Properties) > 0 {
			if err := json.Unmarshal(rf.Properties, &props); err != nil {
				return nil, fmt.Errorf("geo: feature %d: parse properties: %w", i, err)
			}
		}
		features = append(features, Feature{Properties: props, Geometry: geometry})
	}
	return features, nil
}

func decodeGeometry(g rawGeometry) (MultiPolygon, error) {
	switch g.Type {
	case "Polygon":
		var rings [][][2]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil {
			return nil, fmt.Errorf("decode Polygon coordinates: %w", err)
		}
		return MultiPolygon{polygonFromRaw(rings)}, nil
	case "MultiPolygon":
		var polys [][][][2]float64
		if err := json.Unmarshal(g.Coordinates, &polys); err != nil {
			return nil, fmt.Errorf("decode MultiPolygon coordinates: %w", err)
		}
		mp := make(MultiPolygon, len(polys))
		for i, rings := range polys {
			mp[i] = polygonFromRaw(rings)
		}
		return mp, nil
	default:
		return nil, fmt.Errorf("unsupported geometry type %q (only Polygon/MultiPolygon)", g.Type)
	}
}

func polygonFromRaw(rings [][][2]float64) Polygon {
	out := make([]Ring, len(rings))
	for i, r := range rings {
		ring := make(Ring, len(r))
		for j, p := range r {
			ring[j] = Point{p[0], p[1]}
		}
		out[i] = ring
	}
	return Polygon{Rings: out}
}

type geoJSONFeatureCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
}

type geoJSONFeature struct {
	Type       string             `json:"type"`
	Properties map[string]any     `json:"properties"`
	Geometry   geoJSONGeometryOut `json:"geometry"`
}

type geoJSONGeometryOut struct {
	Type        string           `json:"type"`
	Coordinates [][][][2]float64 `json:"coordinates"`
}

// coordinatePrecision rounds output coordinates to roughly 1cm at the
// equator. It hides float round-trip noise (e.g. Simplify's meters
// projection and back) without discarding any meaningful precision for
// polygon data of this kind.
const coordinatePrecision = 1e7

// RoundCoordinate rounds a single lon/lat value to coordinatePrecision, for
// exporters (like TopoJSON) that build their own coordinate arrays instead
// of going through ToGeoJSONGeometry.
func RoundCoordinate(v float64) float64 {
	return math.Round(v*coordinatePrecision) / coordinatePrecision
}

// ToGeoJSONGeometry renders mp as a GeoJSON MultiPolygon geometry — always
// that type, even for a single part, so every feature in a written
// collection shares one consistent geometry type.
func ToGeoJSONGeometry(mp MultiPolygon) geoJSONGeometryOut {
	coords := make([][][][2]float64, len(mp))
	for i, poly := range mp {
		rings := make([][][2]float64, len(poly.Rings))
		for j, r := range poly.Rings {
			ring := make([][2]float64, len(r))
			for k, p := range r {
				ring[k] = [2]float64{RoundCoordinate(p[0]), RoundCoordinate(p[1])}
			}
			rings[j] = ring
		}
		coords[i] = rings
	}
	if coords == nil {
		coords = [][][][2]float64{}
	}
	return geoJSONGeometryOut{Type: "MultiPolygon", Coordinates: coords}
}

// WriteFeatureCollection encodes features as an indented GeoJSON
// FeatureCollection. map[string]any properties marshal with sorted keys, so
// output is byte-for-byte deterministic given the same input.
func WriteFeatureCollection(features []Feature) ([]byte, error) {
	fc := geoJSONFeatureCollection{Type: "FeatureCollection", Features: make([]geoJSONFeature, len(features))}
	for i, f := range features {
		props := f.Properties
		if props == nil {
			props = map[string]any{}
		}
		fc.Features[i] = geoJSONFeature{Type: "Feature", Properties: props, Geometry: ToGeoJSONGeometry(f.Geometry)}
	}
	return json.MarshalIndent(fc, "", "  ")
}
