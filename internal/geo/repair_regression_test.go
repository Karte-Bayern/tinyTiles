package geo

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func rotateRing(r Ring, start int, reverse bool) Ring {
	n := len(r) - 1
	out := make(Ring, 0, n+1)
	for i := 0; i < n; i++ {
		j := (start + i) % n
		if reverse {
			j = (start - i + n) % n
		}
		out = append(out, r[j])
	}
	return append(out, out[0])
}

func TestPolygonDedupePreservesHolesAndEdges(t *testing.T) {
	outer := square(0, 0, 10, 10)
	a, b := square(1, 1, 2, 2), square(5, 5, 6, 6)
	original := Polygon{Rings: []Ring{outer, a, b}}
	equivalent := Polygon{Rings: []Ring{rotateRing(outer, 2, true), rotateRing(b, 1, true), rotateRing(a, 3, false)}}
	withoutHoles := Polygon{Rings: []Ring{outer}}
	changedHole := Polygon{Rings: []Ring{outer, a, square(7, 7, 8, 8)}}
	out, dropped := DedupePolygons(MultiPolygon{original, equivalent, withoutHoles, changedHole})
	if len(out) != 3 || dropped != 1 {
		t.Fatalf("out=%d dropped=%d", len(out), dropped)
	}
	crossed := Ring{outer[0], outer[2], outer[1], outer[3], outer[0]}
	if ringSignature(outer) == ringSignature(crossed) {
		t.Fatal("edge order lost")
	}
	for i := 0; i < len(outer)-1; i++ {
		for _, reverse := range []bool{false, true} {
			if ringSignature(rotateRing(outer, i, reverse)) != ringSignature(outer) {
				t.Fatalf("rotation %d reverse %v", i, reverse)
			}
		}
	}
	if ringSignature(outer[:len(outer)-1]) != ringSignature(outer) {
		t.Fatal("open ring lost its final vertex")
	}
}

func TestMinimumRotationMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(73))
	less := func(q []quantPoint, a, b int) bool {
		for k := range q {
			x, y := q[(a+k)%len(q)], q[(b+k)%len(q)]
			if x != y {
				return x[0] < y[0] || (x[0] == y[0] && x[1] < y[1])
			}
		}
		return false
	}
	for trial := 0; trial < 1000; trial++ {
		q := make([]quantPoint, 1+rng.Intn(50))
		for i := range q {
			q[i] = quantPoint{int64(rng.Intn(3)), int64(rng.Intn(3))}
		}
		want := 0
		for i := range q {
			if less(q, i, want) {
				want = i
			}
		}
		got := minimumRotation(q)
		if less(q, got, want) || less(q, want, got) {
			t.Fatalf("rotation %d != %d for %v", got, want, q)
		}
	}
}

func TestRepairDoesNotPromoteHole(t *testing.T) {
	out, warnings := Repair(MultiPolygon{{Rings: []Ring{{{0, 0}, {0, 0}}, square(1, 1, 2, 2)}}}, "fixture")
	if len(out) != 0 || len(warnings) == 0 {
		t.Fatalf("out=%v warnings=%v", out, warnings)
	}
}

func TestRepairCountsDistinctVertices(t *testing.T) {
	out, warnings := Repair(MultiPolygon{{Rings: []Ring{{{0, 0}, {1, 1}, {0, 0}, {1, 1}, {0, 0}}}}}, "fixture")
	if len(out) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "distinct") {
		t.Fatalf("out=%v warnings=%v", out, warnings)
	}
}

func TestDedupeDoesNotMutateGeometry(t *testing.T) {
	r := square(0, 0, 2, 2)
	before := append(Ring(nil), r...)
	DedupePolygons(MultiPolygon{{Rings: []Ring{r, r}}})
	if !reflect.DeepEqual(r, before) {
		t.Fatal("input changed")
	}
}
