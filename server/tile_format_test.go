package server

import "testing"

func TestInferTileFormat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		metadataFormat    string
		contentType       string
		extensionOverride string
		wantContentType   string
		wantExtension     string
	}{
		{name: "vector pbf", metadataFormat: "pbf", wantContentType: "application/vnd.mapbox-vector-tile", wantExtension: "mvt"},
		{name: "vector MIME", metadataFormat: "application/vnd.mapbox-vector-tile", wantContentType: "application/vnd.mapbox-vector-tile", wantExtension: "mvt"},
		{name: "png", metadataFormat: "PNG", wantContentType: "image/png", wantExtension: "png"},
		{name: "jpeg", metadataFormat: "jpeg", wantContentType: "image/jpeg", wantExtension: "jpg"},
		{name: "webp", metadataFormat: "webp", wantContentType: "image/webp", wantExtension: "webp"},
		{name: "avif", metadataFormat: "image/avif", wantContentType: "image/avif", wantExtension: "avif"},
		{name: "unknown compatibility", metadataFormat: "terrain", wantContentType: "application/octet-stream", wantExtension: "mvt"},
		{name: "content type override", metadataFormat: "png", contentType: "application/x-custom", wantContentType: "application/x-custom", wantExtension: "png"},
		{name: "extension override", metadataFormat: "png", extensionOverride: ".tiles", wantContentType: "image/png", wantExtension: "tiles"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			format, err := inferTileFormat(map[string]string{"format": test.metadataFormat}, test.contentType, test.extensionOverride)
			if err != nil {
				t.Fatal(err)
			}
			if format.contentType != test.wantContentType || format.extension != test.wantExtension {
				t.Fatalf("inferTileFormat() = %#v, want contentType=%q extension=%q", format, test.wantContentType, test.wantExtension)
			}
		})
	}
}

func TestNormalizeTileExtensionRejectsUnsafeRoutes(t *testing.T) {
	t.Parallel()
	for _, value := range []string{".", "mvt.gz", "../png", "png/extra", "png?x=1", "way-too-long-extension"} {
		if _, err := normalizeTileExtension(value); err == nil {
			t.Fatalf("normalizeTileExtension(%q) succeeded", value)
		}
	}
	if got, err := normalizeTileExtension(".JPG"); err != nil || got != "jpg" {
		t.Fatalf("normalizeTileExtension(.JPG) = %q, %v", got, err)
	}
}

func TestParseXYZPathWithExtension(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path      string
		extension string
		valid     bool
	}{
		{path: "2/1/1.jpg", extension: "jpg", valid: true},
		{path: "2/1/1.webp", extension: "webp", valid: true},
		{path: "2/1/1", extension: "png", valid: true},
		{path: "2/1/1.mvt", extension: "jpg", valid: false},
	} {
		_, _, _, err := parseXYZPathWithExtension(test.path, test.extension)
		if (err == nil) != test.valid {
			t.Fatalf("parseXYZPathWithExtension(%q, %q) error = %v, valid=%t", test.path, test.extension, err, test.valid)
		}
	}
}
