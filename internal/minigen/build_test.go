package minigen

import "testing"

func TestRoadClass(t *testing.T) {
	tests := []struct {
		highway string
		want    string
		minZoom int
		ok      bool
	}{
		{"motorway", "motorway", 5, true},
		{"residential", "residential", 11, true},
		{"cycleway", "path", 14, true},
		{"construction", "", 0, false},
	}
	for _, test := range tests {
		got, minZoom, ok := roadClass(map[string]string{"highway": test.highway})
		if got != test.want || minZoom != test.minZoom || ok != test.ok {
			t.Fatalf("roadClass(%q) = (%q, %d, %t), want (%q, %d, %t)", test.highway, got, minZoom, ok, test.want, test.minZoom, test.ok)
		}
	}
}

func TestAreaClass(t *testing.T) {
	tests := []struct {
		tags    map[string]string
		layer   string
		class   string
		minZoom int
		ok      bool
	}{
		{map[string]string{"building": "yes"}, "building", "building", 14, true},
		{map[string]string{"natural": "water"}, "water", "water", 8, true},
		{map[string]string{"landuse": "forest"}, "landcover", "forest", 9, true},
		{map[string]string{"natural": "wood"}, "landcover", "forest", 9, true},
		{map[string]string{"landuse": "farmland"}, "landcover", "farmland", 10, true},
		{map[string]string{"landuse": "meadow"}, "landcover", "meadow", 10, true},
		{map[string]string{"natural": "grassland"}, "landcover", "grass", 10, true},
		{map[string]string{"natural": "scrub"}, "landcover", "grass", 10, true},
		{map[string]string{"natural": "heath"}, "landcover", "grass", 10, true},
		{map[string]string{"landuse": "grass"}, "landcover", "grass", 10, true},
		{map[string]string{"landuse": "residential"}, "landcover", "urban", 10, true},
		{map[string]string{"landuse": "commercial"}, "landcover", "urban", 10, true},
		{map[string]string{"landuse": "industrial"}, "landcover", "urban", 10, true},
		{map[string]string{"landuse": "retail"}, "landcover", "urban", 10, true},
		{map[string]string{"building": "no"}, "", "", 0, false},
	}
	for _, test := range tests {
		layer, class, minZoom, ok := areaClass(test.tags)
		if layer != test.layer || class != test.class || minZoom != test.minZoom || ok != test.ok {
			t.Fatalf("areaClass(%v) = (%q, %q, %d, %t), want (%q, %q, %d, %t)", test.tags, layer, class, minZoom, ok, test.layer, test.class, test.minZoom, test.ok)
		}
	}
}

func TestXYZToTMS(t *testing.T) {
	if got := xyzToTMS(2, 3); got != 5 {
		t.Fatalf("XYZ y=2 at z=3 converted to %d, want 5", got)
	}
}
