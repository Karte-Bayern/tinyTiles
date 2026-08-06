package server

import "testing"

func TestInferTileFormat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		metadataFormat    string
		metadataEncoding  string
		contentType       string
		extensionOverride string
		encodingOverride  string
		wantContentType   string
		wantExtension     string
		wantEncoding      string
	}{
		{name: "vector pbf", metadataFormat: "pbf", wantContentType: "application/vnd.mapbox-vector-tile", wantExtension: "mvt"},
		{name: "vector MIME", metadataFormat: "application/vnd.mapbox-vector-tile", wantContentType: "application/vnd.mapbox-vector-tile", wantExtension: "mvt"},
		{name: "png", metadataFormat: "PNG", wantContentType: "image/png", wantExtension: "png"},
		{name: "jpeg", metadataFormat: "jpeg", wantContentType: "image/jpeg", wantExtension: "jpg"},
		{name: "webp", metadataFormat: "webp", wantContentType: "image/webp", wantExtension: "webp"},
		{name: "avif", metadataFormat: "image/avif", wantContentType: "image/avif", wantExtension: "avif"},
		{name: "gif", metadataFormat: "GIF", wantContentType: "image/gif", wantExtension: "gif"},
		{name: "tiff", metadataFormat: "tiff", wantContentType: "image/tiff", wantExtension: "tif"},
		{name: "svg", metadataFormat: "svg", wantContentType: "image/svg+xml", wantExtension: "svg"},
		{name: "json", metadataFormat: "json", wantContentType: "application/json", wantExtension: "json"},
		{name: "geojson", metadataFormat: "geojson", wantContentType: "application/geo+json", wantExtension: "geojson"},
		{name: "unknown compatibility", metadataFormat: "terrain", wantContentType: "application/octet-stream", wantExtension: "mvt"},
		{name: "content type override", metadataFormat: "png", contentType: "application/x-custom", wantContentType: "application/x-custom", wantExtension: "png"},
		{name: "extension override", metadataFormat: "png", extensionOverride: ".tiles", wantContentType: "image/png", wantExtension: "tiles"},

		// Raster DEM: the payload stays an ordinary raster, only encoding marks
		// it as elevation data.
		{name: "terrarium format", metadataFormat: "terrarium", wantContentType: "image/png", wantExtension: "png", wantEncoding: "terrarium"},
		{name: "terrain-rgb format maps to mapbox", metadataFormat: "terrain-rgb", wantContentType: "image/png", wantExtension: "png", wantEncoding: "mapbox"},
		{name: "webp terrarium", metadataFormat: "webp-terrarium", wantContentType: "image/webp", wantExtension: "webp", wantEncoding: "terrarium"},
		{name: "plain png with encoding metadata", metadataFormat: "png", metadataEncoding: "terrarium", wantContentType: "image/png", wantExtension: "png", wantEncoding: "terrarium"},
		{name: "encoding metadata alias", metadataFormat: "png", metadataEncoding: "Terrain-RGB", wantContentType: "image/png", wantExtension: "png", wantEncoding: "mapbox"},
		{name: "unknown encoding metadata is dropped", metadataFormat: "png", metadataEncoding: "nonsense", wantContentType: "image/png", wantExtension: "png"},
		{name: "encoding override wins", metadataFormat: "png", metadataEncoding: "terrarium", encodingOverride: "mapbox", wantContentType: "image/png", wantExtension: "png", wantEncoding: "mapbox"},
		{name: "encoding override declares a plain raster", metadataFormat: "png", encodingOverride: "custom", wantContentType: "image/png", wantExtension: "png", wantEncoding: "custom"},
		{name: "encoding is not relayed for vector tiles", metadataFormat: "pbf", metadataEncoding: "terrarium", wantContentType: "application/vnd.mapbox-vector-tile", wantExtension: "mvt"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]string{"format": test.metadataFormat}
			if test.metadataEncoding != "" {
				metadata["encoding"] = test.metadataEncoding
			}
			format, err := inferTileFormat(metadata, test.contentType, test.extensionOverride, test.encodingOverride)
			if err != nil {
				t.Fatal(err)
			}
			if format.contentType != test.wantContentType || format.extension != test.wantExtension || format.encoding != test.wantEncoding {
				t.Fatalf("inferTileFormat() = %#v, want contentType=%q extension=%q encoding=%q", format, test.wantContentType, test.wantExtension, test.wantEncoding)
			}
		})
	}
}

func TestInferTileFormatRejectsUnknownDEMEncodingOverride(t *testing.T) {
	t.Parallel()
	if _, err := inferTileFormat(map[string]string{"format": "png"}, "", "", "nonsense"); err == nil {
		t.Fatal("unknown DEM encoding override accepted")
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
