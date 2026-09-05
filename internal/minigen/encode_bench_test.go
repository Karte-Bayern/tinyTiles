package minigen

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func encodeTileFresh(key tileKey, layers map[string][]feature, tolerance float64) ([]byte, error) {
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

func TestEncodeTileCompressorReuse(t *testing.T) {
	for i := 0; i < 32; i++ {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			t.Parallel()
			layers := map[string][]feature{"transportation": {{kind: geometryLine, points: []point{{-10, 0}, {float64(i + 1), 10}}, properties: map[string]any{"class": "motorway"}}}}
			want, err := encodeTileFresh(tileKey{z: 0}, layers, DefaultSimplifyTolerance)
			if err != nil || len(want) == 0 {
				t.Fatalf("reference: %d bytes, %v", len(want), err)
			}
			for j := 0; j < 4; j++ {
				got, err := encodeTile(tileKey{z: 0}, layers, DefaultSimplifyTolerance)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("reused compressor differs: %v", err)
				}
			}
		})
	}
}

func BenchmarkEncodeTile(b *testing.B) {
	layers := map[string][]feature{"transportation": {{kind: geometryLine, points: []point{{-10, 0}, {10, 10}}, properties: map[string]any{"class": "motorway"}}}}
	for _, tc := range []struct {
		name   string
		encode func(tileKey, map[string][]feature, float64) ([]byte, error)
	}{{"fresh", encodeTileFresh}, {"pooled", encodeTile}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				data, err := tc.encode(tileKey{z: 0}, layers, DefaultSimplifyTolerance)
				if err != nil || len(data) == 0 {
					b.Fatalf("%d bytes: %v", len(data), err)
				}
			}
		})
	}
}
