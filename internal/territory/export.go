package territory

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// Format is an output target. Keep the exporter switch open to more targets
// (MBTiles, PMTiles, vector tiles) without changing the Territory model.
type Format string

const (
	FormatGeoJSON  Format = "geojson"
	FormatTopoJSON Format = "topojson"
	FormatCSV      Format = "csv"
)

// FormatFromExtension infers an output Format from a file extension
// (".geojson"/".json" → GeoJSON, ".topojson" → TopoJSON, ".csv" → CSV).
func FormatFromExtension(ext string) (Format, error) {
	switch ext {
	case ".geojson", ".json":
		return FormatGeoJSON, nil
	case ".topojson":
		return FormatTopoJSON, nil
	case ".csv":
		return FormatCSV, nil
	default:
		return "", fmt.Errorf("territory: cannot infer format from extension %q; pass --format explicitly", ext)
	}
}

// PowerBIPreset trims a build for Power BI's shape-map style visuals:
// stable WGS84 GeoJSON, a default simplification pass when the caller did
// not already ask for one, and source_values dropped since a raw member-key
// list is pure file-size weight a BI visual never reads. Business logic for
// any one BI tool otherwise stays out of the core — this only adjusts
// export shape, not aggregation semantics.
func PowerBIPreset(territories []Territory) []Territory {
	out := make([]Territory, len(territories))
	for i, t := range territories {
		props := make(map[string]any, len(t.Properties))
		for k, v := range t.Properties {
			if k == "source_values" {
				continue
			}
			props[k] = v
		}
		out[i] = Territory{ID: t.ID, Name: t.Name, Geometry: t.Geometry, Properties: props, Members: t.Members}
	}
	return out
}

// DefaultPowerBISimplifyMeters is applied by the CLI when --preset powerbi
// is chosen and the caller did not set --simplify explicitly.
const DefaultPowerBISimplifyMeters = 50

// ToGeoJSON encodes territories as a GeoJSON FeatureCollection.
func ToGeoJSON(territories []Territory) ([]byte, error) {
	features := make([]geo.Feature, len(territories))
	for i, t := range territories {
		features[i] = geo.Feature{Properties: t.Properties, Geometry: t.Geometry}
	}
	return geo.WriteFeatureCollection(features)
}

// ToCSV encodes territory metadata (no geometry) as CSV: one row per
// territory, one column per property key that appears on any territory,
// columns sorted for deterministic output. A property holding a list is
// joined with "|".
func ToCSV(territories []Territory) ([]byte, error) {
	columns := map[string]bool{}
	for _, t := range territories {
		for k := range t.Properties {
			columns[k] = true
		}
	}
	header := make([]string, 0, len(columns))
	for k := range columns {
		if k != "territory_id" && k != "territory_name" {
			header = append(header, k)
		}
	}
	sort.Strings(header)
	header = append([]string{"territory_id", "territory_name"}, header...)

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, t := range territories {
		row := make([]string, len(header))
		for i, col := range header {
			row[i] = csvValue(t.Properties[col])
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func csvValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = csvValue(item)
		}
		out := ""
		for i, p := range parts {
			if i > 0 {
				out += "|"
			}
			out += p
		}
		return out
	default:
		return fmt.Sprint(x)
	}
}
