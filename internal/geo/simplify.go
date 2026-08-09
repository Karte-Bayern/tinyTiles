package geo

import (
	"math"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/minigen/simplify"
)

// Simplify runs Visvalingam-Whyatt generalization (mapshaper.org's default
// method) on every ring of mp, independently. toleranceMeters is a rough
// length scale — internally squared into a triangle-area threshold in a
// local equirectangular meters projection centered on the geometry, so the
// same value behaves consistently regardless of latitude.
//
// Each ring is simplified on its own, so a border shared by two territories
// in the same output file is not guaranteed to simplify identically on both
// sides; this trades exact shared-edge topology for a simple, independent
// per-ring pass, which is sufficient for its purpose here — shrinking file
// size for map rendering — without a full topology-preserving simplifier.
func Simplify(mp MultiPolygon, toleranceMeters float64) MultiPolygon {
	if toleranceMeters <= 0 {
		return mp
	}
	meanLat := meanLatitude(mp)
	cosLat := math.Cos(meanLat * math.Pi / 180)
	if math.Abs(cosLat) < 1e-9 {
		return mp // degenerate near-polar projection; skip rather than divide by ~0
	}
	const degToM = math.Pi / 180 * earthRadiusKM * 1000
	tolerance := toleranceMeters * toleranceMeters

	out := make(MultiPolygon, len(mp))
	for pi, poly := range mp {
		rings := make([]Ring, len(poly.Rings))
		for ri, r := range poly.Rings {
			rings[ri] = simplifyRing(r, cosLat, degToM, tolerance)
		}
		out[pi] = Polygon{Rings: rings}
	}
	return out
}

func simplifyRing(r Ring, cosLat, degToM, tolerance float64) Ring {
	if len(r) < 2 {
		return r
	}
	open := r[:len(r)-1] // drop the closing duplicate; Ring is a closed loop
	pts := make([]simplify.Point, len(open))
	for i, p := range open {
		pts[i] = simplify.Point{p[0] * cosLat * degToM, p[1] * degToM}
	}
	reduced := simplify.Ring(pts, tolerance)
	out := make(Ring, len(reduced)+1)
	for i, p := range reduced {
		out[i] = Point{p[0] / (cosLat * degToM), p[1] / degToM}
	}
	out[len(reduced)] = out[0]
	return out
}
