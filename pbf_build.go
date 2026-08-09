package tinytiles

import tiles "github.com/SimonWaldherr/tinySQL/tiles"

const (
	// DefaultPBFBuildMinZoom and DefaultPBFBuildMaxZoom produce a compact
	// offline basemap that starts with major roads and gains local detail at
	// higher zooms.
	DefaultPBFBuildMinZoom = 5
	DefaultPBFBuildMaxZoom = 14

	// DefaultPBFBuildBatchSize, DefaultPBFBuildMaxMemoryBytes and
	// DefaultPBFBuildMinFreeBytes are conservative defaults for an embedded
	// native build. Applications with a known resource budget may override
	// them in PBFBuildOptions.
	DefaultPBFBuildBatchSize      = 1_000
	DefaultPBFBuildMaxMemoryBytes = 256 << 20
	DefaultPBFBuildMinFreeBytes   = 1 << 30
)

// PBFBuildOptions controls a self-contained OSM PBF to .ttiles build.
//
// BuildPBF always uses tinyTiles' built-in minimal generator. It creates only
// a temporary tile stream, feeds it into the artifact writer, then
// publishes the requested artifact after tinySQL has fully validated it.
// PBFInputs and ArtifactPath are required.
//
// When both MinZoom and MaxZoom are zero, BuildPBF uses the documented default
// range. Other zero-valued resource options use their DefaultPBFBuild* value.
// Set Schema to tiles.SchemaAuto, tiles.SchemaFlat, or
// tiles.SchemaNormalized; an empty value means tiles.SchemaAuto.
type PBFBuildOptions struct {
	PBFInputs    []string
	ArtifactPath string

	MinZoom     int
	MaxZoom     int
	Concurrency int

	// PostalCodes adds a "postal_code" vector layer assembled from OSM
	// boundary=postal_code relations, and writes a GeoJSON sidecar file of
	// the same boundaries next to ArtifactPath (see
	// PBFBuildResult.PostalCodesPath) — directly usable as `tinytiles
	// territory --input` input. It costs an extra PBF scan pass, so it is
	// opt-in.
	PostalCodes bool

	Schema         tiles.Schema
	BatchSize      int
	MaxMemoryBytes int64
	MinFreeBytes   int64

	// ReplaceExisting atomically replaces an already published artifact. On a
	// generation, import, or validation error, the earlier artifact is left in
	// place.
	ReplaceExisting bool

	// Progress is called synchronously at generation boundaries and with each
	// import progress update. Keep it short and do not call BuildPBF from it.
	Progress func(PBFBuildProgress)
}

// PBFBuildProgress reports a high-level generation phase or a bounded import
// phase. For preflight, import, and published phases, Import carries the
// corresponding tinySQL progress record.
//
// Phase is one of "generate", "generated", "preflight", "import", or
// "published". Future additive phases may be introduced, so applications
// should display unknown values rather than treating them as errors.
type PBFBuildProgress struct {
	Phase  string
	Import *tiles.Progress
}

// PBFBounds is the WGS84 extent of renderable geometry seen while building the
// built-in source tileset.
type PBFBounds struct {
	MinLon, MinLat float64
	MaxLon, MaxLat float64
}

// PBFBuildResult is returned only after the generated artifact has been fully
// validated and atomically published. GeneratedTiles and RoadFeatures describe
// the temporary built-in tile stream; Info and Estimate describe the
// published .ttiles artifact.
type PBFBuildResult struct {
	ArtifactPath   string
	Info           tiles.ArtifactInfo
	Estimate       tiles.ResourceEstimate
	RoadFeatures   int
	GeneratedTiles int
	// PostalCodeCount and PostalCodesPath are set only when
	// PBFBuildOptions.PostalCodes was requested and at least one
	// boundary=postal_code relation was assembled: PostalCodesPath names the
	// written GeoJSON sidecar file.
	PostalCodeCount int
	PostalCodesPath string
	Bounds          PBFBounds
}
