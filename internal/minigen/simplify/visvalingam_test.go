package simplify

import "testing"

func TestLinePreservesEndpoints(t *testing.T) {
	points := []Point{{0, 0}, {1, 0.01}, {2, 0}, {3, 0.01}, {4, 0}}
	out := Line(points, 1)
	if len(out) < 2 {
		t.Fatalf("Line must keep at least 2 points, got %d", len(out))
	}
	if out[0] != points[0] {
		t.Errorf("first point changed: got %v want %v", out[0], points[0])
	}
	if out[len(out)-1] != points[len(points)-1] {
		t.Errorf("last point changed: got %v want %v", out[len(out)-1], points[len(points)-1])
	}
}

func TestLineRemovesNegligibleWiggle(t *testing.T) {
	// The interior points sit almost exactly on the line from (0,0) to
	// (4,0); each forms a tiny triangle area well under the tolerance.
	points := []Point{{0, 0}, {1, 0.001}, {2, -0.001}, {3, 0.001}, {4, 0}}
	out := Line(points, 1)
	if len(out) != 2 {
		t.Fatalf("expected the wiggle to collapse to 2 points, got %d: %v", len(out), out)
	}
}

func TestLineKeepsSignificantVertex(t *testing.T) {
	// The middle point forms a large triangle (a real corner), well above
	// tolerance, and must survive.
	points := []Point{{0, 0}, {2, 10}, {4, 0}}
	out := Line(points, 1)
	if len(out) != 3 {
		t.Fatalf("expected the corner to survive, got %d points: %v", len(out), out)
	}
}

func TestLineZeroToleranceIsNoop(t *testing.T) {
	points := []Point{{0, 0}, {1, 0.001}, {2, 0}}
	out := Line(points, 0)
	if len(out) != len(points) {
		t.Fatalf("zero tolerance must not remove points, got %d want %d", len(out), len(points))
	}
}

func TestRingKeepsMinimumTriangle(t *testing.T) {
	// A near-degenerate square where every vertex has a tiny effective area
	// relative to a huge tolerance must still leave a valid 3-point ring.
	points := []Point{{0, 0}, {1, 0.001}, {1, 1}, {0, 1}}
	out := Ring(points, 1e9)
	if len(out) != 3 {
		t.Fatalf("Ring must keep at least 3 points, got %d: %v", len(out), out)
	}
}

func TestRingRemovesCollinearVertex(t *testing.T) {
	// (5,0) sits almost exactly on the edge between (0,0) and (10,0).
	points := []Point{{0, 0}, {5, 0.001}, {10, 0}, {10, 10}, {0, 10}}
	out := Ring(points, 1)
	for _, p := range out {
		if p == (Point{5, 0.001}) {
			t.Fatalf("expected the collinear vertex to be removed, got %v", out)
		}
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 points after removing the collinear vertex, got %d: %v", len(out), out)
	}
}

func TestRingPreservesWindingOrder(t *testing.T) {
	points := []Point{{0, 0}, {5, 0.001}, {10, 0}, {10, 10}, {0, 10}}
	before := signedArea(points)
	out := Ring(points, 1)
	after := signedArea(out)
	if (before < 0) != (after < 0) {
		t.Fatalf("winding order flipped: before=%v after=%v", before, after)
	}
}

func signedArea(points []Point) float64 {
	var sum float64
	n := len(points)
	for i := 0; i < n; i++ {
		a, b := points[i], points[(i+1)%n]
		sum += a[0]*b[1] - b[0]*a[1]
	}
	return sum / 2
}

func TestSmallInputsUnchanged(t *testing.T) {
	line := []Point{{0, 0}, {1, 1}}
	if out := Line(line, 100); len(out) != 2 {
		t.Fatalf("2-point line must be unchanged, got %v", out)
	}
	ring := []Point{{0, 0}, {1, 0}, {0, 1}}
	if out := Ring(ring, 100); len(out) != 3 {
		t.Fatalf("3-point ring must be unchanged, got %v", out)
	}
}
