package geo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
)

// Repair closes unclosed rings, drops consecutive duplicate points, discards
// degenerate rings (fewer than 3 distinct points), and normalizes winding.
// It returns the cleaned geometry plus a human-readable warning for every
// ring it had to alter or drop, so callers (validate/inspect) can surface
// them without failing the whole build over minor source defects.
func Repair(mp MultiPolygon, context string) (MultiPolygon, []string) {
	var warnings []string
	out := make(MultiPolygon, 0, len(mp))
	for pi, poly := range mp {
		var rings []Ring
		for ri, r := range poly.Rings {
			cleaned, warn, ok := repairRing(r)
			if warn != "" {
				warnings = append(warnings, fmt.Sprintf("%s: polygon %d ring %d: %s", context, pi, ri, warn))
			}
			if !ok {
				// An interior ring cannot replace a missing exterior.
				if ri == 0 {
					break
				}
				continue
			}
			rings = append(rings, cleaned)
		}
		if len(rings) == 0 {
			continue
		}
		out = append(out, Polygon{Rings: rings})
	}
	return Normalize(out), warnings
}

func repairRing(r Ring) (Ring, string, bool) {
	if len(r) == 0 {
		return nil, "empty ring", false
	}
	deduped := make(Ring, 0, len(r))
	for _, p := range r {
		if len(deduped) > 0 && deduped[len(deduped)-1] == p {
			continue
		}
		deduped = append(deduped, p)
	}
	warn := ""
	if len(deduped) < 2 || deduped[0] != deduped[len(deduped)-1] {
		if len(deduped) > 0 && deduped[0] != deduped[len(deduped)-1] {
			deduped = append(deduped, deduped[0])
			warn = "ring was not closed; closed automatically"
		}
	}
	// Only three distinct vertices are needed; count them without a map.
	var first [3]Point
	distinct := 0
	for _, p := range deduped {
		seen := false
		for _, q := range first[:distinct] {
			if p == q {
				seen = true
				break
			}
		}
		if !seen {
			first[distinct] = p
			distinct++
			if distinct == 3 {
				break
			}
		}
	}
	if distinct < 3 {
		if warn != "" {
			warn += "; "
		}
		return nil, warn + "fewer than 3 distinct points, dropped", false
	}
	return deduped, warn, true
}

// DedupePolygons drops polygons in mp that are geometrically identical (same
// exterior and hole rings up to rotation and direction) to one already kept, so an
// input FeatureCollection with literal duplicate rows does not double-count
// area or edges during Dissolve.
func DedupePolygons(mp MultiPolygon) (MultiPolygon, int) {
	seen := make(map[string]bool, len(mp))
	out := make(MultiPolygon, 0, len(mp))
	dropped := 0
	for _, poly := range mp {
		sig := polygonSignature(poly)
		if seen[sig] {
			dropped++
			continue
		}
		seen[sig] = true
		out = append(out, poly)
	}
	return out, dropped
}

// polygonSignature includes sorted, length-delimited hole signatures so
// different interiors cannot collapse into the same polygon.
func polygonSignature(poly Polygon) string {
	exterior := ringSignature(poly.Exterior())
	if len(poly.Rings) <= 1 {
		return exterior
	}
	holes := make([]string, 0, len(poly.Rings)-1)
	for _, hole := range poly.Holes() {
		holes = append(holes, ringSignature(hole))
	}
	sort.Strings(holes)
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(exterior)))
	out = append(out, exterior...)
	for _, hole := range holes {
		out = binary.LittleEndian.AppendUint64(out, uint64(len(hole)))
		out = append(out, hole...)
	}
	// This has odd length; bare ring signatures have length divisible by 16.
	return "h" + string(out)
}

// ringSignature preserves edge order while ignoring the start vertex and
// winding. Binary coordinates avoid per-vertex decimal formatting.
func ringSignature(r Ring) string {
	if len(r) == 0 {
		return ""
	}
	pts := r
	if len(pts) > 1 && quantize(pts[0]) == quantize(pts[len(pts)-1]) {
		pts = pts[:len(pts)-1]
	}
	q := make([]quantPoint, len(pts))
	for i, p := range pts {
		q[i] = quantize(p)
	}
	encode := func() []byte {
		start := minimumRotation(q)
		out := make([]byte, 0, len(q)*16)
		for i := range q {
			p := q[(start+i)%len(q)]
			out = binary.LittleEndian.AppendUint64(out, uint64(p[0]))
			out = binary.LittleEndian.AppendUint64(out, uint64(p[1]))
		}
		return out
	}
	forward := encode()
	slices.Reverse(q)
	backward := encode()
	if bytes.Compare(forward, backward) > 0 {
		return string(backward)
	}
	return string(forward)
}

// minimumRotation finds the lexicographically least rotation in linear time,
// including rings with repeated vertices (where a minimum-vertex scan ties).
func minimumRotation(q []quantPoint) int {
	n := len(q)
	i, j, k := 0, 1, 0
	for i < n && j < n && k < n {
		a, b := q[(i+k)%n], q[(j+k)%n]
		if a == b {
			k++
			continue
		}
		if a[0] > b[0] || (a[0] == b[0] && a[1] > b[1]) {
			i += k + 1
			if i == j {
				i++
			}
		} else {
			j += k + 1
			if i == j {
				j++
			}
		}
		k = 0
	}
	return min(i, j)
}
