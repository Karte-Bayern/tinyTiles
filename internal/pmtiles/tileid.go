package pmtiles

import (
	"fmt"
	"math/bits"
)

// MaxZoom bounds a decoded tile coordinate. It matches the zoom ceiling used
// elsewhere in tinyTiles and keeps the Hilbert arithmetic below far away from
// uint64 overflow in 3*id+1.
const MaxZoom = 30

// PMTiles addresses tiles by their cumulative position on a series of Hilbert
// curves, one per zoom level. The conversions below follow the PMTiles v3
// specification and match the reference implementation in
// protomaps/go-pmtiles (BSD-3-Clause); TestTileIDMatchesSpecificationTable
// pins them to the coordinate/ID pairs published in the specification.
//
// The coordinates are XYZ (slippy-map, origin top left), *not* the TMS rows
// tinyTiles stores. Callers converting to MBTiles or a tinyTiles artifact must
// flip the row exactly once at that boundary.
func rotate(n, x, y, rx, ry uint32) (uint32, uint32) {
	if ry == 0 {
		if rx != 0 {
			x = n - 1 - x
			y = n - 1 - y
		}
		return y, x
	}
	return x, y
}

// ZxyToID converts XYZ tile coordinates to their Hilbert TileID.
func ZxyToID(z uint8, x, y uint32) uint64 {
	if z == 0 {
		return 0
	}
	var acc = (uint64(1)<<(z*2) - 1) / 3
	n := uint32(z) - 1
	for s := uint32(1) << n; s > 0; s >>= 1 {
		rx := s & x
		ry := s & y
		acc += uint64((3*rx)^ry) << n
		x, y = rotate(s, x, y, rx, ry)
		n--
	}
	return acc
}

// IDToZxy converts a Hilbert TileID back to XYZ tile coordinates. It rejects
// an identifier whose implied zoom exceeds MaxZoom rather than returning a
// coordinate no reader could serve.
func IDToZxy(id uint64) (uint8, uint32, uint32, error) {
	if id > maxTileID {
		return 0, 0, 0, fmt.Errorf("tile id %d is above zoom %d", id, MaxZoom)
	}
	z := uint8(bits.Len64(3*id+1)-1) / 2
	if z > MaxZoom {
		return 0, 0, 0, fmt.Errorf("tile id %d implies zoom %d, above %d", id, z, MaxZoom)
	}
	acc := (uint64(1)<<(z*2) - 1) / 3
	t := id - acc
	var tx, ty uint32
	for a := uint8(0); a < z; a++ {
		s := uint32(1) << a
		rx := 1 & (uint32(t) >> 1)
		ry := 1 & (uint32(t) ^ rx)
		tx, ty = rotate(s, tx, ty, rx, ry)
		tx += rx << a
		ty += ry << a
		t >>= 2
	}
	return z, tx, ty, nil
}

// maxTileID is the last identifier belonging to MaxZoom: the first identifier
// of the next zoom, minus one.
var maxTileID = (uint64(1)<<((MaxZoom+1)*2)-1)/3 - 1
