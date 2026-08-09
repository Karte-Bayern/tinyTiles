package territory

import (
	"math"
	"sort"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// FeatureInspection summarizes one GeoJSON feature's geometry.
type FeatureInspection struct {
	Index      int
	ID         string
	Components int
	AreaKM2    float64
	BBox       [4]float64
	HasBBox    bool
}

// Inspection summarizes a whole GeoJSON FeatureCollection — either a
// territory build's output, or a raw input file being previewed before a
// build (both are just Polygon/MultiPolygon FeatureCollections).
type Inspection struct {
	FeatureCount int
	PropertyKeys []string
	TotalAreaKM2 float64
	BBox         [4]float64
	HasBBox      bool
	Features     []FeatureInspection
}

// featureID picks a human-readable label for a feature: territory_id, then
// id, then the first property in sorted key order, then its index.
func featureID(index int, properties map[string]any) string {
	for _, key := range []string{"territory_id", "id"} {
		if v, ok := properties[key]; ok {
			return stringify(v)
		}
	}
	for _, key := range sortedAnyKeys(properties) {
		return stringify(properties[key])
	}
	return stringify(index)
}

func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Inspect summarizes features for `territory inspect`.
func Inspect(features []geo.Feature) Inspection {
	keySet := map[string]bool{}
	report := Inspection{FeatureCount: len(features)}
	report.Features = make([]FeatureInspection, len(features))
	minLon, minLat := math.Inf(1), math.Inf(1)
	maxLon, maxLat := math.Inf(-1), math.Inf(-1)

	for i, f := range features {
		for k := range f.Properties {
			keySet[k] = true
		}
		area := geo.AreaKM2(f.Geometry)
		report.TotalAreaKM2 += area
		fi := FeatureInspection{Index: i, ID: featureID(i, f.Properties), Components: len(f.Geometry), AreaKM2: roundTo(area, 4)}
		if bbox, ok := geo.BBox(f.Geometry); ok {
			fi.BBox, fi.HasBBox = bbox, true
			minLon, minLat = math.Min(minLon, bbox[0]), math.Min(minLat, bbox[1])
			maxLon, maxLat = math.Max(maxLon, bbox[2]), math.Max(maxLat, bbox[3])
			report.HasBBox = true
		}
		report.Features[i] = fi
	}
	if report.HasBBox {
		report.BBox = [4]float64{minLon, minLat, maxLon, maxLat}
	}
	report.TotalAreaKM2 = roundTo(report.TotalAreaKM2, 4)
	for k := range keySet {
		report.PropertyKeys = append(report.PropertyKeys, k)
	}
	sort.Strings(report.PropertyKeys)
	return report
}
