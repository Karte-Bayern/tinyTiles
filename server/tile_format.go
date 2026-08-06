package server

import (
	"errors"
	"strings"
)

const defaultTileExtension = "mvt"

// tileFormat is the HTTP representation inferred from an MBTiles format
// value. MBTiles intentionally stores the compact format name rather than an
// HTTP media type; the server translates the common raster and vector values
// once at startup, not on every tile request.
type tileFormat struct {
	contentType string
	extension   string
}

var knownTileFormats = map[string]tileFormat{
	"pbf":                                {contentType: "application/vnd.mapbox-vector-tile", extension: "mvt"},
	"mvt":                                {contentType: "application/vnd.mapbox-vector-tile", extension: "mvt"},
	"application/vnd.mapbox-vector-tile": {contentType: "application/vnd.mapbox-vector-tile", extension: "mvt"},
	"png":                                {contentType: "image/png", extension: "png"},
	"image/png":                          {contentType: "image/png", extension: "png"},
	"jpg":                                {contentType: "image/jpeg", extension: "jpg"},
	"jpeg":                               {contentType: "image/jpeg", extension: "jpg"},
	"image/jpg":                          {contentType: "image/jpeg", extension: "jpg"},
	"image/jpeg":                         {contentType: "image/jpeg", extension: "jpg"},
	"webp":                               {contentType: "image/webp", extension: "webp"},
	"image/webp":                         {contentType: "image/webp", extension: "webp"},
	"avif":                               {contentType: "image/avif", extension: "avif"},
	"image/avif":                         {contentType: "image/avif", extension: "avif"},
}

// inferTileFormat translates the standard MBTiles metadata format into the
// browser-visible MIME type and filename extension. Unknown formats keep the
// old generic MIME and .mvt URL behavior, preserving compatibility for
// bespoke datasets until callers explicitly choose a TileExtension.
func inferTileFormat(metadata map[string]string, contentTypeOverride, extensionOverride string) (tileFormat, error) {
	format := tileFormat{contentType: "application/octet-stream", extension: defaultTileExtension}
	if inferred, found := knownTileFormats[strings.ToLower(strings.TrimSpace(metadata["format"]))]; found {
		format = inferred
	}
	if contentType := strings.TrimSpace(contentTypeOverride); contentType != "" {
		format.contentType = contentType
	}
	if extensionOverride == "" {
		return format, nil
	}
	extension, err := normalizeTileExtension(extensionOverride)
	if err != nil {
		return tileFormat{}, err
	}
	format.extension = extension
	return format, nil
}

// normalizeTileExtension only accepts a compact, URL-safe file extension.
// The extension is inserted into TileJSON URLs, so rejecting separators and
// punctuation here prevents a configuration typo from changing the route.
func normalizeTileExtension(value string) (string, error) {
	value = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "."))
	if value == "" || len(value) > 16 {
		return "", errors.New("tinytiles server: tile extension must contain 1 to 16 lowercase letters or digits")
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return "", errors.New("tinytiles server: tile extension must contain 1 to 16 lowercase letters or digits")
		}
	}
	return value, nil
}
