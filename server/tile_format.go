package server

import (
	"errors"
	"strings"
)

const defaultTileExtension = "mvt"

// vectorTileContentType is the MIME type of a Mapbox/MapLibre-style vector
// tile. tileJSON uses it to decide whether to relay vector_layers metadata:
// that field is meaningless, and must not be sent, for a raster tileset.
const vectorTileContentType = "application/vnd.mapbox-vector-tile"

// Elevation encodings for a raster DEM tileset. A DEM tile is an ordinary
// PNG or WebP on the wire; only this value tells a client how to decode its
// pixels back into metres, so a tileset that omits it renders as meaningless
// colored noise in a terrain/hillshade layer.
const (
	// EncodingTerrarium is Mapzen/Nextzen Terrarium elevation encoding.
	EncodingTerrarium = "terrarium"
	// EncodingMapbox is Mapbox Terrain-RGB elevation encoding. MapLibre and
	// Mapbox GL JS both spell this encoding "mapbox".
	EncodingMapbox = "mapbox"
	// EncodingCustom marks a DEM whose decoding parameters the client supplies
	// itself; it is accepted so an operator can declare a non-standard DEM
	// rather than leave it indistinguishable from an ordinary raster tileset.
	EncodingCustom = "custom"
)

// knownDEMEncodings is the accepted set for a declared elevation encoding.
// An unrecognized value is dropped rather than relayed: emitting an encoding
// a client cannot interpret is worse than omitting the field, which leaves
// the tileset as a plain raster.
var knownDEMEncodings = map[string]string{
	EncodingTerrarium: EncodingTerrarium,
	EncodingMapbox:    EncodingMapbox,
	"terrain-rgb":     EncodingMapbox,
	"terrainrgb":      EncodingMapbox,
	EncodingCustom:    EncodingCustom,
}

// tileFormat is the HTTP representation inferred from an MBTiles format
// value. MBTiles intentionally stores the compact format name rather than an
// HTTP media type; the server translates the common raster and vector values
// once at startup, not on every tile request.
type tileFormat struct {
	contentType string
	extension   string
	// encoding is the raster DEM elevation encoding, empty for every ordinary
	// vector or raster tileset. It is advertised in TileJSON so a client can
	// build a correct terrain source without a side channel.
	encoding string
}

var knownTileFormats = map[string]tileFormat{
	"pbf":                                {contentType: vectorTileContentType, extension: "mvt"},
	"mvt":                                {contentType: vectorTileContentType, extension: "mvt"},
	"application/vnd.mapbox-vector-tile": {contentType: vectorTileContentType, extension: "mvt"},
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
	"gif":                                {contentType: "image/gif", extension: "gif"},
	"image/gif":                          {contentType: "image/gif", extension: "gif"},
	"tif":                                {contentType: "image/tiff", extension: "tif"},
	"tiff":                               {contentType: "image/tiff", extension: "tif"},
	"image/tiff":                         {contentType: "image/tiff", extension: "tif"},
	"svg":                                {contentType: "image/svg+xml", extension: "svg"},
	"image/svg+xml":                      {contentType: "image/svg+xml", extension: "svg"},
	"json":                               {contentType: "application/json", extension: "json"},
	"application/json":                   {contentType: "application/json", extension: "json"},
	"geojson":                            {contentType: "application/geo+json", extension: "geojson"},
	"application/geo+json":               {contentType: "application/geo+json", extension: "geojson"},

	// Raster DEM tilesets. The payload is an ordinary PNG or WebP, so the
	// media type and extension stay the raster ones; only encoding marks the
	// tileset as elevation data.
	"terrarium":      {contentType: "image/png", extension: "png", encoding: EncodingTerrarium},
	"terrain-rgb":    {contentType: "image/png", extension: "png", encoding: EncodingMapbox},
	"terrainrgb":     {contentType: "image/png", extension: "png", encoding: EncodingMapbox},
	"terrain-rgb-v2": {contentType: "image/png", extension: "png", encoding: EncodingMapbox},
	"webp-terrarium": {contentType: "image/webp", extension: "webp", encoding: EncodingTerrarium},
}

// inferTileFormat translates the standard MBTiles metadata format into the
// browser-visible MIME type and filename extension. Unknown formats keep the
// old generic MIME and .mvt URL behavior, preserving compatibility for
// bespoke datasets until callers explicitly choose a TileExtension.
//
// A DEM encoding may come from the format name itself (for example
// "terrarium"), from a generator-written "encoding" metadata row, or from an
// explicit Config.DEMEncoding. Terrain sources very commonly record only
// format=png, so the metadata row and the override are the practical way to
// declare an existing tileset as elevation data without rebuilding it.
func inferTileFormat(metadata map[string]string, contentTypeOverride, extensionOverride, encodingOverride string) (tileFormat, error) {
	format := tileFormat{contentType: "application/octet-stream", extension: defaultTileExtension}
	if inferred, found := knownTileFormats[strings.ToLower(strings.TrimSpace(metadata["format"]))]; found {
		format = inferred
	}
	if declared, found := knownDEMEncodings[strings.ToLower(strings.TrimSpace(metadata["encoding"]))]; found {
		format.encoding = declared
	}
	if override := strings.ToLower(strings.TrimSpace(encodingOverride)); override != "" {
		declared, found := knownDEMEncodings[override]
		if !found {
			return tileFormat{}, errors.New("tinytiles server: DEM encoding must be terrarium, mapbox or custom")
		}
		format.encoding = declared
	}
	if contentType := strings.TrimSpace(contentTypeOverride); contentType != "" {
		format.contentType = contentType
	}
	// An elevation encoding describes how to decode raster pixels. Relaying it
	// for a vector tileset would advertise a terrain source a client cannot
	// build, so drop it rather than emit a contradictory TileJSON.
	if format.contentType == vectorTileContentType {
		format.encoding = ""
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
