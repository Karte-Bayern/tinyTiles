// Package geo provides the small, dependency-free polygon model shared by
// tinyTiles' postal-code index and territory builder: GeoJSON Polygon and
// MultiPolygon types, ring repair and winding normalization, area and bbox
// helpers, and a deterministic dissolve/union for grouping polygons into
// territories.
package geo

import "math"

// Point is a [longitude, latitude] coordinate in WGS84 degrees.
type Point [2]float64

// Ring is a closed sequence of points: the first and last points are equal.
// A Polygon's first Ring is its exterior; any further rings are holes.
type Ring []Point

// Polygon is one exterior ring plus zero or more hole rings.
type Polygon struct {
	Rings []Ring
}

// MultiPolygon is any number of polygons, connected or not.
type MultiPolygon []Polygon

// Exterior returns a polygon's outer boundary, or nil for a polygon with no
// rings at all.
func (p Polygon) Exterior() Ring {
	if len(p.Rings) == 0 {
		return nil
	}
	return p.Rings[0]
}

// Holes returns a polygon's interior rings.
func (p Polygon) Holes() []Ring {
	if len(p.Rings) <= 1 {
		return nil
	}
	return p.Rings[1:]
}

// shoelaceSum is twice the signed area of ring using the standard
// lon=x/lat=y planar convention: positive for a counterclockwise ring,
// negative for clockwise. It is not itself an area (see AreaKM2 for that);
// callers only need its sign to classify winding.
func shoelaceSum(r Ring) float64 {
	var sum float64
	for i := 0; i+1 < len(r); i++ {
		a, b := r[i], r[i+1]
		sum += a[0]*b[1] - b[0]*a[1]
	}
	return sum
}

// IsClockwise reports whether ring is wound clockwise (lon=x/lat=y
// convention, so a hole per RFC 7946).
func IsClockwise(r Ring) bool { return shoelaceSum(r) < 0 }

// reversed returns a copy of r with point order reversed; r[0] stays the
// closing duplicate's partner so the ring is still closed afterward.
func reversed(r Ring) Ring {
	out := make(Ring, len(r))
	for i, p := range r {
		out[len(r)-1-i] = p
	}
	return out
}

// OrientRing returns r wound clockwise (if clockwise) or counterclockwise
// (if not), reversing it only if needed. Exported for producers that
// classify a ring's exterior/hole role from outside information (an
// explicit OSM "outer"/"inner" role, say) rather than inferring it from
// existing winding the way Normalize does.
func OrientRing(r Ring, clockwise bool) Ring {
	if IsClockwise(r) == clockwise {
		return r
	}
	return reversed(r)
}

// Normalize returns mp with every exterior ring forced counterclockwise and
// every hole forced clockwise, the RFC 7946 winding convention that AreaKM2,
// Dissolve and the GeoJSON writer all rely on.
func Normalize(mp MultiPolygon) MultiPolygon {
	out := make(MultiPolygon, len(mp))
	for pi, poly := range mp {
		rings := make([]Ring, len(poly.Rings))
		for ri, r := range poly.Rings {
			wantHole := ri > 0
			if IsClockwise(r) != wantHole {
				r = reversed(r)
			}
			rings[ri] = r
		}
		out[pi] = Polygon{Rings: rings}
	}
	return out
}

// earthRadiusKM is the mean Earth radius used by the equirectangular area
// approximation below.
const earthRadiusKM = 6371.0088

// AreaKM2 returns mp's total area (exterior rings minus holes) in square
// kilometers, using an equirectangular projection scaled by the cosine of
// the geometry's mean latitude. That approximation is accurate to a small
// fraction of a percent for territory-sized polygons (up to a few hundred
// kilometers across) without pulling in a geodesic-area library; it is not
// intended for polar or planet-spanning geometry.
func AreaKM2(mp MultiPolygon) float64 {
	meanLat := meanLatitude(mp)
	cosLat := math.Cos(meanLat * math.Pi / 180)
	var total float64
	for _, poly := range mp {
		for ri, r := range poly.Rings {
			area := math.Abs(shoelaceSum(projectRing(r, cosLat))) / 2
			if ri == 0 {
				total += area
			} else {
				total -= area
			}
		}
	}
	return total
}

func projectRing(r Ring, cosLat float64) Ring {
	out := make(Ring, len(r))
	const degToKM = math.Pi / 180 * earthRadiusKM
	for i, p := range r {
		out[i] = Point{p[0] * cosLat * degToKM, p[1] * degToKM}
	}
	return out
}

func meanLatitude(mp MultiPolygon) float64 {
	var sum float64
	var n int
	for _, poly := range mp {
		ext := poly.Exterior()
		for _, p := range ext {
			sum += p[1]
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// BBox returns mp's [minLon, minLat, maxLon, maxLat] extent. The second
// result is false for an empty geometry.
func BBox(mp MultiPolygon) ([4]float64, bool) {
	minLon, minLat := math.Inf(1), math.Inf(1)
	maxLon, maxLat := math.Inf(-1), math.Inf(-1)
	found := false
	for _, poly := range mp {
		for _, p := range poly.Exterior() {
			found = true
			minLon, minLat = math.Min(minLon, p[0]), math.Min(minLat, p[1])
			maxLon, maxLat = math.Max(maxLon, p[0]), math.Max(maxLat, p[1])
		}
	}
	if !found {
		return [4]float64{}, false
	}
	return [4]float64{minLon, minLat, maxLon, maxLat}, true
}

// Contains reports whether p lies within mp with its holes excluded, using
// the even-odd rule summed across every ring. That correctly treats a hole
// as an exclusion without tracking which exterior it belongs to: entering a
// hole crosses one extra ring boundary, flipping parity back to "outside".
func Contains(mp MultiPolygon, p Point) bool {
	inside := false
	for _, poly := range mp {
		for _, r := range poly.Rings {
			if ringContainsPoint(r, p) {
				inside = !inside
			}
		}
	}
	return inside
}

// ringContainsPoint reports whether p falls inside ring using the standard
// even-odd ray-casting test.
func ringContainsPoint(r Ring, p Point) bool {
	inside := false
	for i, j := 0, len(r)-1; i < len(r); j, i = i, i+1 {
		yi, yj := r[i][1], r[j][1]
		xi, xj := r[i][0], r[j][0]
		if (yi > p[1]) != (yj > p[1]) {
			xCross := (xj-xi)*(p[1]-yi)/(yj-yi) + xi
			if p[0] < xCross {
				inside = !inside
			}
		}
	}
	return inside
}
