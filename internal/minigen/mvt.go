package minigen

import (
	"bytes"
	"compress/gzip"
	"math"
	"sort"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/minigen/simplify"
)

const (
	geometryLine    = 2
	geometryPolygon = 3
	tileExtent      = 4096

	// DefaultSimplifyTolerance is a Visvalingam-Whyatt effective-area
	// threshold in squared tile pixels (mapshaper.org's default
	// simplification method), used when Config.SimplifyTolerance is zero or
	// negative. It is applied post-projection, in the tile's fixed
	// 4096-unit extent, so the same tolerance is imperceptible at every zoom
	// while discarding proportionally more geometry at low zooms, where each
	// tile pixel spans far more ground distance. A larger tolerance produces
	// smaller, coarser tiles; a smaller one preserves more detail at the
	// cost of size.
	DefaultSimplifyTolerance = 4.0
)

type feature struct {
	kind       int
	points     []point
	rings      []ring // multi-ring polygon (postal_code); when set, points is ignored
	properties map[string]any
}

// ring is one exterior or interior member of a multi-ring polygon feature,
// in unprojected WGS84 points. hole marks an interior (donut) ring.
type ring struct {
	points []point
	hole   bool
}

func encodeTile(key tileKey, layers map[string][]feature, tolerance float64) ([]byte, error) {
	var tile []byte
	for _, name := range []string{"water", "landcover", "building", "transportation", "postal_code"} {
		if data := encodeLayer(name, key, layers[name], tolerance); len(data) > 0 {
			tile = appendMessage(tile, 3, data)
		}
	}
	if len(tile) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(tile); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func encodeLayer(name string, key tileKey, features []feature, tolerance float64) []byte {
	if len(features) == 0 {
		return nil
	}
	keys := map[string]uint32{}
	values := map[string]uint32{}
	var keyList, valueList []string
	var encoded [][]byte
	for _, f := range features {
		geom := encodeGeometry(f, key, tolerance)
		if len(geom) == 0 {
			continue
		}
		var tags []byte
		propertyNames := make([]string, 0, len(f.properties))
		for k := range f.properties {
			propertyNames = append(propertyNames, k)
		}
		sort.Strings(propertyNames)
		for _, property := range propertyNames {
			value, ok := f.properties[property].(string)
			if !ok {
				continue
			}
			ki, ok := keys[property]
			if !ok {
				ki = uint32(len(keyList))
				keys[property] = ki
				keyList = append(keyList, property)
			}
			vi, ok := values[value]
			if !ok {
				vi = uint32(len(valueList))
				values[value] = vi
				valueList = append(valueList, value)
			}
			tags = appendVarint(tags, uint64(ki))
			tags = appendVarint(tags, uint64(vi))
		}
		var encodedFeature []byte
		if len(tags) > 0 {
			encodedFeature = appendMessage(encodedFeature, 2, tags)
		}
		encodedFeature = appendVarintField(encodedFeature, 3, uint64(f.kind))
		encodedFeature = appendMessage(encodedFeature, 4, geom)
		encoded = append(encoded, encodedFeature)
	}
	if len(encoded) == 0 {
		return nil
	}
	var layer []byte
	layer = appendMessage(layer, 1, []byte(name))
	for _, f := range encoded {
		layer = appendMessage(layer, 2, f)
	}
	for _, k := range keyList {
		layer = appendMessage(layer, 3, []byte(k))
	}
	for _, v := range valueList {
		layer = appendMessage(layer, 4, encodeValue(v))
	}
	layer = appendVarintField(layer, 5, tileExtent)
	layer = appendVarintField(layer, 15, 2)
	return layer
}

func encodeValue(s string) []byte { return appendMessage(nil, 1, []byte(s)) }

func encodeGeometry(f feature, key tileKey, tolerance float64) []byte {
	if f.kind == geometryPolygon && len(f.rings) > 0 {
		return encodeRings(f.rings, key, tolerance)
	}
	points := make([][2]int, 0, len(f.points))
	for _, p := range f.points {
		points = append(points, projectPoint(p, key))
	}
	if f.kind == geometryLine {
		points = simplifyPixels(points, tolerance, false)
		points = clipLine(points)
		if len(points) < 2 {
			return nil
		}
		return commandGeometry(points, false)
	}
	if f.kind == geometryPolygon {
		if len(points) > 1 && points[0] == points[len(points)-1] {
			points = points[:len(points)-1]
		}
		points = simplifyPixels(points, tolerance, true)
		points = clipPolygon(points)
		if len(points) < 3 {
			return nil
		}
		return commandGeometry(points, true)
	}
	return nil
}

// encodeRings emits one multi-ring polygon geometry: every ring is
// projected, simplified, clipped and wound independently, then chained into
// the same MoveTo/LineTo/ClosePath command stream. A ring's fill role
// (exterior vs. hole) comes entirely from its winding direction, so emission
// order does not need to pair a hole with its containing exterior ring — a
// point-in-tile rasterizer resolves that from the accumulated windings.
func encodeRings(rings []ring, key tileKey, tolerance float64) []byte {
	var out []byte
	last := [2]int{}
	wrote := false
	for _, r := range rings {
		points := make([][2]int, 0, len(r.points))
		for _, p := range r.points {
			points = append(points, projectPoint(p, key))
		}
		if len(points) > 1 && points[0] == points[len(points)-1] {
			points = points[:len(points)-1]
		}
		points = simplifyPixels(points, tolerance, true)
		points = clipPolygon(points)
		if len(points) < 3 {
			continue
		}
		points = ensureWinding(points, !r.hole)
		out, last = appendRing(out, points, last, true)
		wrote = true
	}
	if !wrote {
		return nil
	}
	return out
}

// ensureWinding reverses points if needed so its signed area's sign matches
// clockwise (the Mapbox Vector Tile Spec's required exterior-ring winding in
// a tile's Y-down pixel space; holes take the opposite winding).
func ensureWinding(points [][2]int, clockwise bool) [][2]int {
	if (signedArea(points) > 0) == clockwise {
		return points
	}
	reversed := make([][2]int, len(points))
	for i, p := range points {
		reversed[len(points)-1-i] = p
	}
	return reversed
}

func signedArea(points [][2]int) float64 {
	var sum float64
	for i, a := range points {
		b := points[(i+1)%len(points)]
		sum += float64(a[0])*float64(b[1]) - float64(b[0])*float64(a[1])
	}
	return sum
}

// simplifyPixels runs Visvalingam-Whyatt simplification on already-projected
// tile-pixel coordinates, rounding back to the integer grid the rest of the
// encoder expects.
func simplifyPixels(points [][2]int, tolerance float64, closed bool) [][2]int {
	in := make([]simplify.Point, len(points))
	for i, p := range points {
		in[i] = simplify.Point{float64(p[0]), float64(p[1])}
	}
	var out []simplify.Point
	if closed {
		out = simplify.Ring(in, tolerance)
	} else {
		out = simplify.Line(in, tolerance)
	}
	result := make([][2]int, len(out))
	for i, p := range out {
		result[i] = [2]int{int(math.Round(p[0])), int(math.Round(p[1]))}
	}
	return result
}

func projectPoint(p point, key tileKey) [2]int {
	lat := math.Max(-85.05112878, math.Min(85.05112878, p[1]))
	n := math.Exp2(float64(key.z))
	x := ((p[0]+180)/360*n - float64(key.x)) * tileExtent
	y := ((1-math.Log(math.Tan(lat*math.Pi/180)+1/math.Cos(lat*math.Pi/180))/math.Pi)/2*n - float64(key.y)) * tileExtent
	return [2]int{int(math.Round(x)), int(math.Round(y))}
}

func commandGeometry(points [][2]int, polygon bool) []byte {
	out, _ := appendRing(nil, points, [2]int{}, polygon)
	return out
}

// appendRing emits MoveTo + LineTo* (+ ClosePath when close) for one ring or
// line, continuing the running delta-cursor from last, and returns the
// extended buffer plus the cursor's new position.
func appendRing(out []byte, points [][2]int, last [2]int, close bool) ([]byte, [2]int) {
	out = appendVarint(out, 9)
	out = appendPoint(out, points[0], last)
	last = points[0]
	if len(points) > 1 {
		out = appendVarint(out, uint64((len(points)-1)<<3|2))
		for _, p := range points[1:] {
			out = appendPoint(out, p, last)
			last = p
		}
	}
	if close {
		out = appendVarint(out, 15)
	}
	return out, last
}
func appendPoint(out []byte, p, last [2]int) []byte {
	return appendVarint(appendVarint(out, zigzagEncode(int64(p[0]-last[0]))), zigzagEncode(int64(p[1]-last[1])))
}
func zigzagEncode(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// Keep geometry inside the tile. Lines use a lightweight segment clip; polygons
// use Sutherland-Hodgman clipping against the four tile edges.
func clipLine(in [][2]int) (out [][2]int) {
	for i := 1; i < len(in); i++ {
		a, b, ok := clipSegment(in[i-1], in[i])
		if !ok {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != a {
			out = append(out, a)
		}
		if out[len(out)-1] != b {
			out = append(out, b)
		}
	}
	return
}
func clipSegment(a, b [2]int) ([2]int, [2]int, bool) {
	x0, y0, x1, y1 := float64(a[0]), float64(a[1]), float64(b[0]), float64(b[1])
	dx, dy := x1-x0, y1-y0
	t0, t1 := 0.0, 1.0
	for _, c := range [][2]float64{{-dx, x0}, {dx, float64(tileExtent) - x0}, {-dy, y0}, {dy, float64(tileExtent) - y0}} {
		p, q := c[0], c[1]
		if p == 0 {
			if q < 0 {
				return a, b, false
			}
			continue
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return a, b, false
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return a, b, false
			}
			if r < t1 {
				t1 = r
			}
		}
	}
	return [2]int{int(math.Round(x0 + t0*dx)), int(math.Round(y0 + t0*dy))}, [2]int{int(math.Round(x0 + t1*dx)), int(math.Round(y0 + t1*dy))}, true
}
func clipPolygon(in [][2]int) [][2]int {
	out := in
	for edge := 0; edge < 4; edge++ {
		if len(out) == 0 {
			return nil
		}
		next := make([][2]int, 0, len(out))
		for i, a := range out {
			b := out[(i+1)%len(out)]
			ai, bi := inside(a, edge), inside(b, edge)
			if ai && bi {
				next = append(next, b)
			} else if ai && !bi {
				_, q, _ := clipSegment(a, b)
				next = append(next, q)
			} else if !ai && bi {
				q, _, _ := clipSegment(a, b)
				next = append(next, q, b)
			}
		}
		out = next
	}
	return out
}
func inside(p [2]int, edge int) bool {
	switch edge {
	case 0:
		return p[0] >= 0
	case 1:
		return p[0] <= tileExtent
	case 2:
		return p[1] >= 0
	default:
		return p[1] <= tileExtent
	}
}

func appendMessage(dst []byte, field int, value []byte) []byte {
	dst = appendVarint(dst, uint64(field<<3|2))
	dst = appendVarint(dst, uint64(len(value)))
	return append(dst, value...)
}
func appendVarintField(dst []byte, field int, value uint64) []byte {
	dst = appendVarint(dst, uint64(field<<3))
	return appendVarint(dst, value)
}
func appendVarint(dst []byte, v uint64) []byte {
	for v >= 128 {
		dst = append(dst, byte(v)|128)
		v >>= 7
	}
	return append(dst, byte(v))
}
