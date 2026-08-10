package server

import "testing"

func vectorMetadata(layers ...string) map[string]string {
	json := `{"vector_layers":[`
	for i, id := range layers {
		if i > 0 {
			json += ","
		}
		json += `{"id":"` + id + `","fields":{}}`
	}
	json += `]}`
	return map[string]string{"minzoom": "5", "maxzoom": "14", "name": "test", "json": json}
}

func TestBuildStyleVectorLayersMatchDataset(t *testing.T) {
	metadata := vectorMetadata("water", "landcover", "building", "transportation", "postal_code")
	style := buildStyle(metadata, "https://tiles.example/tiles/{z}/{x}/{y}.mvt", vectorTileContentType)

	if style["version"] != 8 {
		t.Fatalf("version = %v, want 8", style["version"])
	}
	sources, ok := style["sources"].(map[string]any)
	if !ok {
		t.Fatalf("sources = %#v", style["sources"])
	}
	source, ok := sources["tinytiles"].(map[string]any)
	if !ok || source["type"] != "vector" {
		t.Fatalf("tinytiles source = %#v", sources["tinytiles"])
	}

	layers, ok := style["layers"].([]any)
	if !ok {
		t.Fatalf("layers = %#v", style["layers"])
	}
	ids := layerIDs(t, layers)
	for _, want := range []string{"background", "water", "landcover", "transportation", "building", "postal_code-line"} {
		if !ids[want] {
			t.Errorf("missing layer %q in %v", want, ids)
		}
	}
	if ids["postal_code-label"] {
		// MapLibre GL JS refuses to load any style with a text-field symbol
		// layer unless it also declares a "glyphs" font URL; tinyTiles does
		// not host font glyphs, so no layer here may use text-field.
		t.Error("postal_code-label must not exist: it would need a glyphs URL this style does not provide")
	}
}

func TestBuildStyleOmitsLayersNotInDataset(t *testing.T) {
	// Only transportation exists: no water/building/postal_code paint rules
	// should be invented for layers the dataset never declared.
	metadata := vectorMetadata("transportation")
	style := buildStyle(metadata, "https://tiles.example/tiles/{z}/{x}/{y}.mvt", vectorTileContentType)
	ids := layerIDs(t, style["layers"].([]any))
	for _, unwanted := range []string{"water", "landcover", "building", "postal_code-line"} {
		if ids[unwanted] {
			t.Errorf("unexpected layer %q for a transportation-only dataset", unwanted)
		}
	}
	if !ids["transportation"] {
		t.Error("expected the transportation layer to be present")
	}
}

func TestBuildStyleRasterDataset(t *testing.T) {
	metadata := map[string]string{"minzoom": "0", "maxzoom": "12", "name": "dem"}
	style := buildStyle(metadata, "https://tiles.example/tiles/{z}/{x}/{y}.png", "image/png")
	sources, ok := style["sources"].(map[string]any)
	if !ok {
		t.Fatalf("sources = %#v", style["sources"])
	}
	source, ok := sources["tinytiles"].(map[string]any)
	if !ok || source["type"] != "raster" {
		t.Fatalf("tinytiles source = %#v", sources["tinytiles"])
	}
	ids := layerIDs(t, style["layers"].([]any))
	if !ids["tinytiles-raster"] {
		t.Errorf("expected a raster layer, got %v", ids)
	}
	if ids["water"] || ids["transportation"] {
		t.Errorf("a raster style must not include vector paint layers, got %v", ids)
	}
}

// TestBuildStyleNeverUsesTextFieldWithoutGlyphs guards against the failure
// a real MapLibre GL JS load caught during development: any symbol layer
// using "text-field" without the style also declaring "glyphs" makes
// MapLibre refuse the whole style, not just that layer.
func TestBuildStyleNeverUsesTextFieldWithoutGlyphs(t *testing.T) {
	metadata := vectorMetadata("water", "landcover", "building", "transportation", "postal_code")
	style := buildStyle(metadata, "https://tiles.example/tiles/{z}/{x}/{y}.mvt", vectorTileContentType)
	if _, hasGlyphs := style["glyphs"]; hasGlyphs {
		return // a style declaring glyphs may freely use text-field
	}
	for _, raw := range style["layers"].([]any) {
		layer := raw.(map[string]any)
		layout, ok := layer["layout"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasTextField := layout["text-field"]; hasTextField {
			t.Errorf("layer %q uses text-field but the style declares no glyphs URL", layer["id"])
		}
	}
}

func layerIDs(t *testing.T, layers []any) map[string]bool {
	t.Helper()
	ids := make(map[string]bool, len(layers))
	for _, raw := range layers {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("layer is not an object: %#v", raw)
		}
		id, ok := m["id"].(string)
		if !ok {
			t.Fatalf("layer has no string id: %#v", m)
		}
		ids[id] = true
	}
	return ids
}
