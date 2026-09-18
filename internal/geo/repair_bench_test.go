package geo

import (
	"math"
	"testing"
)

func BenchmarkRingSignature(b *testing.B) {
	r := make(Ring, 1001)
	for i := 0; i < 1000; i++ {
		a := float64(i) * 2 * math.Pi / 1000
		r[i] = Point{math.Cos(a), math.Sin(a)}
	}
	r[1000] = r[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ringSignature(r) == "" {
			b.Fatal("empty signature")
		}
	}
}
