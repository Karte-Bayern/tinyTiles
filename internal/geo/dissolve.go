package geo

import "math"

// quantScale rounds coordinates to roughly 1cm at the equator before two
// edges are compared for cancellation. Polygons built from a shared source
// (the usual case: postal-code or admin boundaries digitized once and reused
// by every adjacent feature) already share exact vertices; this only guards
// against trivial floating point noise.
const quantScale = 1e7

type quantPoint [2]int64

func quantize(p Point) quantPoint {
	return quantPoint{int64(math.Round(p[0] * quantScale)), int64(math.Round(p[1] * quantScale))}
}

// Dissolve unions a group's member polygons into one MultiPolygon by
// canceling directed edges that appear in both directions across the
// collected rings — the shared border between two touching polygons in the
// same group, traced in opposite directions because both rings wind
// consistently. It is not a general polygon-boolean engine: overlapping (as
// opposed to edge-adjacent) input in the same group is not resolved, and
// Validate's overlap check exists to surface that case instead. Within that
// scope it correctly handles touching boundaries, holes, and disconnected
// components, and members is deduplicated first so literal duplicate
// geometries do not distort the result.
func Dissolve(members []MultiPolygon) (MultiPolygon, error) {
	var flattened MultiPolygon
	for _, mp := range members {
		flattened = append(flattened, mp...)
	}
	flattened = Normalize(flattened)
	flattened, _ = DedupePolygons(flattened)
	if len(flattened) == 0 {
		return nil, nil
	}

	var rings []Ring
	for _, poly := range flattened {
		rings = append(rings, poly.Rings...)
	}

	surviving := cancelSharedEdges(rings)
	chained := chainEdges(surviving)
	return classifyRings(chained), nil
}

type directedEdge struct {
	a, b   Point
	qa, qb quantPoint
}

// cancelSharedEdges returns every directed edge from rings whose reverse
// does not also occur somewhere in rings. A non-self-intersecting ring never
// repeats a directed edge, so a reverse match can only come from a
// different, adjacent ring's shared border.
func cancelSharedEdges(rings []Ring) []directedEdge {
	present := make(map[quantPoint]map[quantPoint]bool)
	mark := func(a, b quantPoint) {
		if present[a] == nil {
			present[a] = make(map[quantPoint]bool)
		}
		present[a][b] = true
	}
	var edges []directedEdge
	for _, r := range rings {
		for i := 0; i+1 < len(r); i++ {
			a, b := r[i], r[i+1]
			qa, qb := quantize(a), quantize(b)
			if qa == qb {
				continue
			}
			edges = append(edges, directedEdge{a, b, qa, qb})
			mark(qa, qb)
		}
	}
	surviving := edges[:0]
	for _, e := range edges {
		if present[e.qb] != nil && present[e.qb][e.qa] {
			continue // canceled by a matching reverse edge elsewhere
		}
		surviving = append(surviving, e)
	}
	return surviving
}

// chainEdges reconnects surviving directed edges, which all point the same
// way around whatever ring they end up forming, into closed rings.
func chainEdges(edges []directedEdge) []Ring {
	byStart := make(map[quantPoint][]int, len(edges))
	for i, e := range edges {
		byStart[e.qa] = append(byStart[e.qa], i)
	}
	used := make([]bool, len(edges))

	var rings []Ring
	for start := range edges {
		if used[start] {
			continue
		}
		ring := Ring{edges[start].a}
		current := start
		for {
			used[current] = true
			ring = append(ring, edges[current].b)
			if edges[current].qb == edges[start].qa {
				break // closed
			}
			next := -1
			for _, cand := range byStart[edges[current].qb] {
				if !used[cand] {
					next = cand
					break
				}
			}
			if next < 0 {
				break // could not close; keep whatever chained so far below
			}
			current = next
		}
		if len(ring) >= 4 && quantize(ring[0]) == quantize(ring[len(ring)-1]) {
			rings = append(rings, ring)
		}
	}
	return rings
}

// classifyRings splits chained rings into exteriors and holes by winding,
// then delegates to NestHoles to rebuild valid Polygon boundaries.
func classifyRings(rings []Ring) MultiPolygon {
	var exteriors, holes []Ring
	for _, r := range rings {
		if IsClockwise(r) {
			holes = append(holes, r)
		} else {
			exteriors = append(exteriors, r)
		}
	}
	return NestHoles(exteriors, holes)
}

// NestHoles builds Polygons by nesting each hole ring under its innermost
// containing exterior ring — required for GeoJSON, where (unlike a vector
// tile's fill-rule rendering) a hole must be listed under the right
// exterior. It is exported so a producer that already knows which of its
// rings are exterior vs. hole (for example from explicit OSM multipolygon
// "outer"/"inner" roles, rather than winding inferred by Dissolve) can reuse
// the same nesting logic instead of duplicating it.
func NestHoles(exteriors, holes []Ring) MultiPolygon {
	polys := make([]Polygon, len(exteriors))
	for i, ext := range exteriors {
		polys[i] = Polygon{Rings: []Ring{ext}}
	}
	for _, hole := range holes {
		best := -1
		var bestArea float64
		for i, ext := range exteriors {
			if len(hole) == 0 || !ringContainsPoint(ext, hole[0]) {
				continue
			}
			area := math.Abs(shoelaceSum(ext))
			if best < 0 || area < bestArea {
				best, bestArea = i, area
			}
		}
		if best >= 0 {
			polys[best].Rings = append(polys[best].Rings, hole)
		}
		// A hole with no containing exterior has nothing to subtract from
		// (e.g. its owning exterior was dropped as degenerate); dropping it
		// too is correct rather than a defect.
	}
	return MultiPolygon(polys)
}
