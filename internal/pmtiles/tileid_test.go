package pmtiles

import "testing"

// TestTileIDMatchesSpecificationTable pins the Hilbert mapping to the
// coordinate/identifier pairs published in the PMTiles v3 specification. It is
// independent ground truth: the values come from the specification, not from
// this implementation. The z12 entry in particular exercises twelve rounds of
// the rotation, so an error in the curve would not survive it.
func TestTileIDMatchesSpecificationTable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{z: 0, x: 0, y: 0, id: 0},
		{z: 1, x: 0, y: 0, id: 1},
		{z: 1, x: 0, y: 1, id: 2},
		{z: 1, x: 1, y: 1, id: 3},
		{z: 1, x: 1, y: 0, id: 4},
		{z: 2, x: 0, y: 0, id: 5},
		{z: 12, x: 3423, y: 1763, id: 19078479},
	} {
		if got := ZxyToID(test.z, test.x, test.y); got != test.id {
			t.Errorf("ZxyToID(%d, %d, %d) = %d, want %d", test.z, test.x, test.y, got, test.id)
		}
		z, x, y, err := IDToZxy(test.id)
		if err != nil {
			t.Errorf("IDToZxy(%d): %v", test.id, err)
			continue
		}
		if z != test.z || x != test.x || y != test.y {
			t.Errorf("IDToZxy(%d) = %d/%d/%d, want %d/%d/%d", test.id, z, x, y, test.z, test.x, test.y)
		}
	}
}

// TestTileIDRoundTripsEveryCoordinateThroughZoom8 checks the two conversions
// are exact inverses across whole zoom levels, and that each zoom occupies a
// contiguous identifier block with no collisions or gaps.
func TestTileIDRoundTripsEveryCoordinateThroughZoom8(t *testing.T) {
	t.Parallel()
	for z := uint8(0); z <= 8; z++ {
		side := uint32(1) << z
		base := (uint64(1)<<(z*2) - 1) / 3
		seen := make(map[uint64]bool, int(side)*int(side))
		for x := uint32(0); x < side; x++ {
			for y := uint32(0); y < side; y++ {
				id := ZxyToID(z, x, y)
				if id < base || id >= base+uint64(side)*uint64(side) {
					t.Fatalf("ZxyToID(%d,%d,%d) = %d, outside this zoom's block", z, x, y, id)
				}
				if seen[id] {
					t.Fatalf("ZxyToID(%d,%d,%d) = %d collides with another coordinate", z, x, y, id)
				}
				seen[id] = true
				gotZ, gotX, gotY, err := IDToZxy(id)
				if err != nil {
					t.Fatalf("IDToZxy(%d): %v", id, err)
				}
				if gotZ != z || gotX != x || gotY != y {
					t.Fatalf("round trip %d/%d/%d -> %d -> %d/%d/%d", z, x, y, id, gotZ, gotX, gotY)
				}
			}
		}
		if uint64(len(seen)) != uint64(side)*uint64(side) {
			t.Fatalf("zoom %d produced %d ids, want %d", z, len(seen), uint64(side)*uint64(side))
		}
	}
}

func TestIDToZxyRejectsIdentifiersAboveMaxZoom(t *testing.T) {
	t.Parallel()
	if _, _, _, err := IDToZxy(maxTileID); err != nil {
		t.Fatalf("last supported tile id rejected: %v", err)
	}
	if _, _, _, err := IDToZxy(maxTileID + 1); err == nil {
		t.Fatal("tile id above the supported zoom accepted")
	}
}
