package minigen

// This file assembles OSM boundary=postal_code relations (the same
// multipolygon convention suche-postleitzahl.org's PLZ boundaries and
// MapTiler's postcode layers use) into a dedicated postal_code vector
// layer, plus the []PostalFeature returned to callers for a standalone
// postcode-boundary GeoJSON sidecar. It only runs when Config.PostalCodes
// is set: a default build pays nothing for it.

import (
	"context"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// postalMinZoom is the lowest zoom the postal_code layer is added at,
// matching the area layers' (water/landcover) region-overview threshold —
// a country's full postcode mesh is not useful much below it.
const postalMinZoom = 8

// PostalFeature is one assembled postal-code boundary.
type PostalFeature struct {
	Code     string
	Name     string
	Geometry geo.MultiPolygon
}

func collectPostalFeatures(ctx context.Context, inputs []string, concurrency int) ([]PostalFeature, error) {
	relations, err := collectPostalRelations(ctx, inputs)
	if err != nil {
		return nil, err
	}
	if len(relations) == 0 {
		return nil, nil
	}

	wantWays := make(map[int64]struct{})
	for _, rel := range relations {
		for _, m := range rel.Members {
			if m.Type == memberWay {
				wantWays[m.Ref] = struct{}{}
			}
		}
	}
	ways, err := collectWaysByID(ctx, inputs, wantWays)
	if err != nil {
		return nil, err
	}

	wantNodes := make(map[int64]struct{})
	for _, w := range ways {
		for _, id := range w.NodeIDs {
			wantNodes[id] = struct{}{}
		}
	}
	coords, _, err := loadCoordinates(ctx, inputs, wantNodes, concurrency)
	if err != nil {
		return nil, err
	}

	features := make([]PostalFeature, 0, len(relations))
	for _, rel := range relations {
		code := postalCode(rel.Tags)
		if code == "" {
			continue
		}
		mp := assemblePostalMultiPolygon(rel, ways, coords)
		if len(mp) == 0 {
			continue
		}
		features = append(features, PostalFeature{
			Code:     code,
			Name:     strings.TrimSpace(rel.Tags["name"]),
			Geometry: mp,
		})
	}
	return features, nil
}

func collectPostalRelations(ctx context.Context, inputs []string) ([]relation, error) {
	var relations []relation
	for _, input := range inputs {
		err := scanPBFRelations(ctx, input, func(r *relation) error {
			if r.Tags["boundary"] == "postal_code" && postalCode(r.Tags) != "" {
				relations = append(relations, *r)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return relations, nil
}

func postalCode(tags map[string]string) string {
	if v := strings.TrimSpace(tags["postal_code"]); v != "" {
		return v
	}
	return strings.TrimSpace(tags["ref"])
}

func collectWaysByID(ctx context.Context, inputs []string, wanted map[int64]struct{}) (map[int64]*way, error) {
	ways := make(map[int64]*way, len(wanted))
	for _, input := range inputs {
		err := scanPBF(ctx, input, 1, func(_ *node, w *way) error {
			if w == nil {
				return nil
			}
			if _, ok := wanted[w.ID]; ok {
				cp := *w
				ways[w.ID] = &cp
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return ways, nil
}

// assemblePostalMultiPolygon turns a boundary=postal_code relation's member
// ways into a geo.MultiPolygon: "outer" (or unset-role) members form
// exterior rings, "inner" members form holes, each assembled from
// potentially several way segments via assembleRings.
func assemblePostalMultiPolygon(rel relation, ways map[int64]*way, coords map[int64]point) geo.MultiPolygon {
	var outerChains, innerChains [][]int64
	for _, m := range rel.Members {
		if m.Type != memberWay {
			continue
		}
		w, ok := ways[m.Ref]
		if !ok || len(w.NodeIDs) < 2 {
			continue
		}
		if m.Role == "inner" {
			innerChains = append(innerChains, w.NodeIDs)
		} else {
			outerChains = append(outerChains, w.NodeIDs)
		}
	}

	toGeoRing := func(ids []int64) geo.Ring {
		line := wayLine(coords, ids)
		ring := make(geo.Ring, 0, len(line))
		for _, p := range line {
			ring = append(ring, geo.Point{p[0], p[1]})
		}
		return ring
	}

	var outers, inners []geo.Ring
	for _, ids := range assembleRings(outerChains) {
		if r := toGeoRing(ids); len(r) >= 4 {
			outers = append(outers, geo.OrientRing(r, false)) // exterior: counterclockwise
		}
	}
	for _, ids := range assembleRings(innerChains) {
		if r := toGeoRing(ids); len(r) >= 4 {
			inners = append(inners, geo.OrientRing(r, true)) // hole: clockwise
		}
	}
	if len(outers) == 0 {
		return nil
	}
	return geo.NestHoles(outers, inners)
}

// addPostalFeatures adds one MVT feature per assembled postal boundary to
// zoom's tile builders, in the "postal_code" layer.
func addPostalFeatures(builders map[tileKey]*tileBuilder, zoom int, features []PostalFeature) {
	for _, pf := range features {
		rings := toMVTRings(pf.Geometry)
		if len(rings) == 0 {
			continue
		}
		properties := map[string]any{"class": "postal_code", "postal_code": pf.Code}
		if pf.Name != "" {
			properties["name"] = pf.Name
		}
		addFeature(builders, zoom, "postal_code", feature{kind: geometryPolygon, rings: rings, properties: properties})
	}
}

func toMVTRings(mp geo.MultiPolygon) []ring {
	var out []ring
	for _, poly := range mp {
		for i, r := range poly.Rings {
			points := make([]point, len(r))
			for j, p := range r {
				points[j] = point{p[0], p[1]}
			}
			out = append(out, ring{points: points, hole: i > 0})
		}
	}
	return out
}
