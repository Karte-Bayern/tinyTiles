package geo

import (
	"fmt"
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
			label := fmt.Sprintf("%s: polygon %d ring %d", context, pi, ri)
			cleaned, warn, ok := repairRing(r)
			if warn != "" {
				warnings = append(warnings, label+": "+warn)
			}
			if !ok {
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
	distinct := len(deduped) - 1
	if distinct < 3 {
		if warn != "" {
			warn += "; "
		}
		return nil, warn + "fewer than 3 distinct points, dropped", false
	}
	return deduped, warn, true
}

// DedupePolygons drops polygons in mp that are geometrically identical (same
// exterior ring up to rotation and direction) to one already kept, so an
// input FeatureCollection with literal duplicate rows does not double-count
// area or edges during Dissolve.
func DedupePolygons(mp MultiPolygon) (MultiPolygon, int) {
	seen := make(map[string]bool, len(mp))
	out := make(MultiPolygon, 0, len(mp))
	dropped := 0
	for _, poly := range mp {
		sig := ringSignature(poly.Exterior())
		if seen[sig] {
			dropped++
			continue
		}
		seen[sig] = true
		out = append(out, poly)
	}
	return out, dropped
}

// ringSignature is a rotation- and direction-invariant fingerprint of a
// ring's vertex set, quantized so trivial floating point noise between two
// otherwise-identical rings does not defeat the comparison.
func ringSignature(r Ring) string {
	if len(r) < 2 {
		return ""
	}
	pts := r[:len(r)-1] // drop the closing duplicate
	quantized := make([]quantPoint, len(pts))
	for i, p := range pts {
		quantized[i] = quantize(p)
	}
	sort.Slice(quantized, func(i, j int) bool {
		if quantized[i][0] != quantized[j][0] {
			return quantized[i][0] < quantized[j][0]
		}
		return quantized[i][1] < quantized[j][1]
	})
	out := make([]byte, 0, len(quantized)*17)
	for _, q := range quantized {
		out = fmt.Appendf(out, "%d,%d;", q[0], q[1])
	}
	return string(out)
}
