package server

import "testing"

func TestParseXYZPath(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path    string
		z, x, y int
		valid   bool
	}{
		{path: "2/1/1", z: 2, x: 1, y: 1, valid: true},
		{path: "2/1/1.mvt", z: 2, x: 1, y: 1, valid: true},
		{path: "/30/1073741823/1073741823", z: 30, x: 1073741823, y: 1073741823, valid: true},
		{path: "", valid: false},
		{path: "2/1", valid: false},
		{path: "2//1", valid: false},
		{path: "2/1/1/", valid: false},
		{path: "2/./1", valid: false},
		{path: "2/1/../1", valid: false},
		{path: "2/1/-1", valid: false},
		{path: "2/1/1.0", valid: false},
		{path: "31/0/0", valid: false},
		{path: "30/1073741824/0", valid: false},
	} {
		test := test
		t.Run(test.path, func(t *testing.T) {
			z, x, y, err := parseXYZPath(test.path)
			if test.valid {
				if err != nil || z != test.z || x != test.x || y != test.y {
					t.Fatalf("parseXYZPath(%q) = %d/%d/%d, %v", test.path, z, x, y, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseXYZPath(%q) = %d/%d/%d, want error", test.path, z, x, y)
			}
		})
	}
}

func BenchmarkParseXYZPath(b *testing.B) {
	const raw = "14/8872/5372.mvt"
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		z, x, y, err := parseXYZPath(raw)
		if err != nil || z != 14 || x != 8872 || y != 5372 {
			b.Fatalf("parseXYZPath = %d/%d/%d, %v", z, x, y, err)
		}
	}
}

func TestTileCoordinateETag(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got, want := tileCoordinateETag(revision, ":xyz:", 30, 1073741823, 0), `"`+revision+`:xyz:30/1073741823/0"`; got != want {
		t.Fatalf("XYZ ETag = %q, want %q", got, want)
	}
	if got, want := tileCoordinateETag(revision, ":tms:", 2, 1, 2), `"`+revision+`:tms:2/1/2"`; got != want {
		t.Fatalf("TMS ETag = %q, want %q", got, want)
	}
}
