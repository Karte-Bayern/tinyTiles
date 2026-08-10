package server

import (
	"encoding/json"
	"net/http"
)

// serveStyle handles GET /style.json: a minimal, ready-to-use MapLibre GL
// style for whatever this dataset actually contains. It follows the same
// precomputed-vs-lazy pattern as serveTileJSON: a generation built with a
// known PublicBase serves its precomputed payload; otherwise the style is
// built per-request from the incoming request's own origin.
func (s *Server) serveStyle(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	gen := s.gen.Load()
	if gen.stylePayload != nil {
		writeJSON(w, request, gen.stylePayload, gen.stylePayloadGzip, gen.styleETag, "public, max-age=300, stale-while-revalidate=60")
		return
	}
	baseURL, err := s.baseURL(request)
	if err != nil {
		http.Error(w, "invalid public base URL", http.StatusBadRequest)
		return
	}
	tileURL := s.xyzTileURL(baseURL, gen)
	payload, err := json.Marshal(buildStyle(gen.metadata, tileURL, gen.contentType))
	if err != nil {
		http.Error(w, "encode style", http.StatusInternalServerError)
		return
	}
	writeJSON(w, request, payload, nil, quoteETag(digest(payload)), "public, max-age=300, stale-while-revalidate=60")
}

// buildStyle renders a MapLibre GL style (version 8): a raster style for a
// raster tileset, or a vector style with paint rules for every well-known
// tinyTiles layer (water/landcover/building/transportation/postal_code)
// that the dataset's vector_layers metadata actually declares. An
// unrecognized vector layer gets a source but no style layer — this
// deliberately does not guess a geometry type for a layer it doesn't know,
// so it stays correct for a non-minigen, externally generated tileset too.
func buildStyle(metadata map[string]string, tileURL, contentType string) map[string]any {
	minZoom := integerMetadata(metadata, "minzoom", 0)
	maxZoom := integerMetadata(metadata, "maxzoom", 22)
	name := metadata["name"]
	if name == "" {
		name = "tinyTiles"
	}
	layers := []any{backgroundStyleLayer()}

	if contentType != vectorTileContentType {
		layers = append(layers, rasterStyleLayer())
		return map[string]any{
			"version": 8,
			"name":    name,
			"sources": map[string]any{
				"tinytiles": map[string]any{
					"type": "raster", "tiles": []string{tileURL}, "tileSize": 256,
					"minzoom": minZoom, "maxzoom": maxZoom,
				},
			},
			"layers": layers,
		}
	}

	present := presentVectorLayers(metadata["json"])
	if present["water"] {
		layers = append(layers, waterStyleLayer())
	}
	if present["landcover"] {
		layers = append(layers, landcoverStyleLayer())
	}
	if present["transportation"] {
		layers = append(layers, transportationStyleLayer())
	}
	if present["building"] {
		layers = append(layers, buildingStyleLayer())
	}
	if present["postal_code"] {
		layers = append(layers, postalCodeStyleLayers()...)
	}
	return map[string]any{
		"version": 8,
		"name":    name,
		"sources": map[string]any{
			"tinytiles": map[string]any{
				"type": "vector", "tiles": []string{tileURL}, "minzoom": minZoom, "maxzoom": maxZoom,
			},
		},
		"layers": layers,
	}
}

func presentVectorLayers(rawJSON string) map[string]bool {
	vectorLayers, _ := vectorTilesetMetadata(rawJSON)
	present := make(map[string]bool, len(vectorLayers))
	for _, raw := range vectorLayers {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := m["id"].(string); ok {
			present[id] = true
		}
	}
	return present
}

func backgroundStyleLayer() map[string]any {
	return map[string]any{
		"id": "background", "type": "background",
		"paint": map[string]any{"background-color": "#f7f4ee"},
	}
}

func rasterStyleLayer() map[string]any {
	return map[string]any{"id": "tinytiles-raster", "type": "raster", "source": "tinytiles"}
}

func waterStyleLayer() map[string]any {
	return map[string]any{
		"id": "water", "type": "fill", "source": "tinytiles", "source-layer": "water",
		"paint": map[string]any{"fill-color": "#a8d0e6"},
	}
}

func landcoverStyleLayer() map[string]any {
	return map[string]any{
		"id": "landcover", "type": "fill", "source": "tinytiles", "source-layer": "landcover",
		"paint": map[string]any{
			"fill-color": []any{
				"match", []any{"get", "class"},
				"forest", "#c9e1b9",
				"farmland", "#eee4c5",
				"meadow", "#ddebc8",
				"#e7e5df",
			},
			"fill-opacity": 0.8,
		},
	}
}

func buildingStyleLayer() map[string]any {
	return map[string]any{
		"id": "building", "type": "fill", "source": "tinytiles", "source-layer": "building",
		"minzoom": 14,
		"paint":   map[string]any{"fill-color": "#d9d0c7", "fill-outline-color": "#c3b8ab"},
	}
}

func transportationStyleLayer() map[string]any {
	return map[string]any{
		"id": "transportation", "type": "line", "source": "tinytiles", "source-layer": "transportation",
		"layout": map[string]any{"line-join": "round", "line-cap": "round"},
		"paint": map[string]any{
			"line-color": []any{
				"match", []any{"get", "class"},
				"motorway", "#e66a5c",
				"trunk", "#ea8a5d",
				"primary", "#f0ad4e",
				"secondary", "#f7d774",
				"tertiary", "#f9e9a0",
				"path", "#b9a98a",
				"#ffffff",
			},
			"line-width": []any{
				"interpolate", []any{"linear"}, []any{"zoom"},
				5, []any{
					"match", []any{"get", "class"},
					"motorway", 1.2,
					"trunk", 1.0,
					0.5,
				},
				14, []any{
					"match", []any{"get", "class"},
					"motorway", 8.0,
					"trunk", 6.0,
					"primary", 5.0,
					"secondary", 4.0,
					"tertiary", 3.0,
					"residential", 2.5,
					"service", 1.5,
					"path", 1.0,
					2.0,
				},
			},
		},
	}
}

// postalCodeStyleLayers draws the postal_code layer the way
// suche-postleitzahl.org presents PLZ boundaries: a dashed outline. A
// code/name label layer is deliberately not included: MapLibre GL JS
// refuses to load any style containing a symbol layer with "text-field"
// unless the style also declares a "glyphs" font-PBF URL, and tinyTiles
// does not host font glyphs — confirmed by loading this style in a real
// MapLibre GL JS map during development, which failed outright with
// exactly that error until the label layer was removed.
func postalCodeStyleLayers() []any {
	return []any{
		map[string]any{
			"id": "postal_code-line", "type": "line", "source": "tinytiles", "source-layer": "postal_code",
			"paint": map[string]any{
				"line-color":     "#8a4fb0",
				"line-width":     1.2,
				"line-dasharray": []any{2.0, 1.0},
			},
		},
	}
}
