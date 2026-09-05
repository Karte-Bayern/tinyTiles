package geo

import "testing"

func BenchmarkCancelSharedEdges(b *testing.B) {
	rings := make([]Ring, 0, 2500)
	for x := 0; x < 50; x++ {
		for y := 0; y < 50; y++ {
			a, c := float64(x), float64(y)
			rings = append(rings, Ring{{a, c}, {a + 1, c}, {a + 1, c + 1}, {a, c + 1}, {a, c}})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(cancelSharedEdges(rings)); got != 200 {
			b.Fatalf("edges=%d", got)
		}
	}
}
