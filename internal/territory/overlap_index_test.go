package territory

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

func bruteOverlaps(opts Options) []string {
	boxes := make([][4]float64, len(opts.Features))
	valid := make([]bool, len(opts.Features))
	for i, f := range opts.Features {
		boxes[i], valid[i] = geo.BBox(f.Geometry)
	}
	var out []string
	for i, a := range opts.Features {
		if !valid[i] {
			continue
		}
		for j := i + 1; j < len(opts.Features); j++ {
			if valid[j] && bboxAreaOverlap(boxes[i], boxes[j]) {
				out = append(out, fmt.Sprintf("%s and %s", stringify(a.Properties[opts.GeometryKey]), stringify(opts.Features[j].Properties[opts.GeometryKey])))
			}
		}
	}
	return SortedUnique(out)
}

func overlapFeature(id int, x, y, width, height float64) geo.Feature {
	return geo.Feature{Properties: map[string]any{"id": fmt.Sprint(id)}, Geometry: geo.MultiPolygon{{Rings: []geo.Ring{{{x, y}, {x + width, y}, {x + width, y + height}, {x, y + height}, {x, y}}}}}}
}

func TestOverlapCandidatesMatchBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 25; trial++ {
		opts := Options{GeometryKey: "id"}
		for i := 0; i < 100; i++ {
			opts.Features = append(opts.Features, overlapFeature(i, float64(rng.Intn(20)), float64(rng.Intn(20)), float64(rng.Intn(5)), float64(rng.Intn(5))))
		}
		opts.Features = append(opts.Features, geo.Feature{})
		if got, want := detectOverlaps(opts), bruteOverlaps(opts); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: got %v want %v", trial, got, want)
		}
	}
}

func BenchmarkDetectOverlaps(b *testing.B) {
	opts := Options{GeometryKey: "id"}
	for i := 0; i < 2000; i++ {
		opts.Features = append(opts.Features, overlapFeature(i, float64(i)*2, 0, 1, 1))
	}
	for _, tc := range []struct {
		name string
		f    func(Options) []string
	}{{"brute", bruteOverlaps}, {"sorted", detectOverlaps}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := tc.f(opts); len(got) != 0 {
					b.Fatal(got)
				}
			}
		})
	}
}
