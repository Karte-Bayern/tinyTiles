package geo

import (
	"math"
	"testing"
)

func square(x0, y0, x1, y1 float64) Ring {
	return Ring{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}
}

func TestDissolveMergesTouchingSquares(t *testing.T) {
	a := MultiPolygon{{Rings: []Ring{square(0, 0, 1, 1)}}}
	b := MultiPolygon{{Rings: []Ring{square(1, 0, 2, 1)}}}

	out, err := Dissolve([]MultiPolygon{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected one merged polygon, got %d: %#v", len(out), out)
	}
	area := AreaKM2(out)
	wantArea := AreaKM2(MultiPolygon{{Rings: []Ring{square(0, 0, 2, 1)}}})
	if math.Abs(area-wantArea) > wantArea*0.001 {
		t.Fatalf("area = %.6f, want ~%.6f", area, wantArea)
	}
	bbox, ok := BBox(out)
	if !ok || bbox != [4]float64{0, 0, 2, 1} {
		t.Fatalf("bbox = %v", bbox)
	}
}

func TestDissolvePreservesHole(t *testing.T) {
	outer := square(0, 0, 4, 4)
	hole := square(1, 1, 3, 3)
	mp := MultiPolygon{{Rings: []Ring{outer, hole}}}

	out, err := Dissolve([]MultiPolygon{mp})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(out[0].Rings) != 2 {
		t.Fatalf("expected one polygon with a hole, got %#v", out)
	}
	area := AreaKM2(out)
	wantArea := AreaKM2(MultiPolygon{{Rings: []Ring{outer}}}) - AreaKM2(MultiPolygon{{Rings: []Ring{hole}}})
	if math.Abs(area-wantArea) > wantArea*0.001 {
		t.Fatalf("area = %.6f, want ~%.6f (hole must be subtracted)", area, wantArea)
	}
}

func TestDissolveKeepsDisconnectedComponents(t *testing.T) {
	a := MultiPolygon{{Rings: []Ring{square(0, 0, 1, 1)}}}
	b := MultiPolygon{{Rings: []Ring{square(10, 10, 11, 11)}}}

	out, err := Dissolve([]MultiPolygon{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 disconnected polygons, got %d: %#v", len(out), out)
	}
}

func TestDissolveDedupesExactDuplicates(t *testing.T) {
	a := MultiPolygon{{Rings: []Ring{square(0, 0, 1, 1)}}}
	dup := MultiPolygon{{Rings: []Ring{square(0, 0, 1, 1)}}}

	out, err := Dissolve([]MultiPolygon{a, dup})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected duplicates to collapse to one polygon, got %d: %#v", len(out), out)
	}
	area := AreaKM2(out)
	wantArea := AreaKM2(a)
	if math.Abs(area-wantArea) > wantArea*0.001 {
		t.Fatalf("area = %.6f, want ~%.6f (duplicate must not double-count)", area, wantArea)
	}
}

func TestDissolveSingleMemberIsUnchanged(t *testing.T) {
	a := MultiPolygon{{Rings: []Ring{square(0, 0, 1, 1)}}}
	out, err := Dissolve([]MultiPolygon{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(out[0].Rings) != 1 {
		t.Fatalf("expected the single input polygon back unchanged, got %#v", out)
	}
}

func TestRepairClosesRingAndDropsDegenerate(t *testing.T) {
	unclosed := Ring{{0, 0}, {1, 0}, {1, 1}, {0, 1}} // missing closing point
	degenerate := Ring{{5, 5}, {5, 5}, {5, 5}}
	mp := MultiPolygon{
		{Rings: []Ring{unclosed}},
		{Rings: []Ring{degenerate}},
	}
	out, warnings := Repair(mp, "test")
	if len(out) != 1 {
		t.Fatalf("expected the degenerate polygon dropped, got %d polygons: %#v", len(out), out)
	}
	ring := out[0].Exterior()
	if ring[0] != ring[len(ring)-1] {
		t.Fatalf("ring was not closed: %v", ring)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (closed + dropped), got %v", warnings)
	}
}

func TestNormalizeFixesWinding(t *testing.T) {
	// Clockwise exterior, counterclockwise hole: both backwards.
	backwardsExterior := reversed(square(0, 0, 4, 4))
	backwardsHole := square(1, 1, 3, 3) // CCW as constructed, wrong for a hole
	mp := MultiPolygon{{Rings: []Ring{backwardsExterior, backwardsHole}}}

	out := Normalize(mp)
	if IsClockwise(out[0].Exterior()) {
		t.Error("exterior should be counterclockwise after Normalize")
	}
	if !IsClockwise(out[0].Holes()[0]) {
		t.Error("hole should be clockwise after Normalize")
	}
}
