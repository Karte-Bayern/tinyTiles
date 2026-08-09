package territory

import (
	"encoding/json"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// ToTopoJSON encodes territories as a basic TopoJSON Topology: valid,
// dependency-free TopoJSON that a standard client (topojson-client, d3-geo)
// can read. Each ring becomes its own arc; arcs are not shared or quantized
// across territories, so this does not get TopoJSON's usual cross-boundary
// deduplication win — building that requires resolving shared edges across
// the whole output, which Dissolve already does per-group but not across
// groups. Prefer simplified GeoJSON when file size matters more than the
// TopoJSON format itself.
func ToTopoJSON(territories []Territory) ([]byte, error) {
	var arcs [][][2]float64
	object := topoObject{Type: "GeometryCollection", Geometries: make([]topoGeometry, len(territories))}
	for i, t := range territories {
		polyArcs := make([][][]int, len(t.Geometry))
		for pi, poly := range t.Geometry {
			ringArcs := make([][]int, len(poly.Rings))
			for ri, ring := range poly.Rings {
				arcIndex := len(arcs)
				coords := make([][2]float64, len(ring))
				for k, p := range ring {
					coords[k] = [2]float64{geo.RoundCoordinate(p[0]), geo.RoundCoordinate(p[1])}
				}
				arcs = append(arcs, coords)
				ringArcs[ri] = []int{arcIndex}
			}
			polyArcs[pi] = ringArcs
		}
		object.Geometries[i] = topoGeometry{Type: "MultiPolygon", Properties: t.Properties, Arcs: polyArcs}
	}
	topo := topoTopology{
		Type:    "Topology",
		Objects: map[string]topoObject{"territories": object},
		Arcs:    arcs,
	}
	if topo.Arcs == nil {
		topo.Arcs = [][][2]float64{}
	}
	return json.MarshalIndent(topo, "", "  ")
}

type topoTopology struct {
	Type    string                `json:"type"`
	Objects map[string]topoObject `json:"objects"`
	Arcs    [][][2]float64        `json:"arcs"`
}

type topoObject struct {
	Type       string         `json:"type"`
	Geometries []topoGeometry `json:"geometries"`
}

type topoGeometry struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Arcs       [][][]int      `json:"arcs"` // [polygon][ring][arcIndex]
}
