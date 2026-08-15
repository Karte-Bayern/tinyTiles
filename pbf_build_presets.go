package tinytiles

import "fmt"

// Preset names a bundle of built-in-generator settings tuned for one of
// tinyTiles' documented use cases, so a caller can pick "mobile" or
// "postcode" instead of hand-tuning MinZoom/MaxZoom/SimplifyTolerance. An
// empty Preset behaves identically to PresetBalanced: today's defaults,
// unchanged. Presets only ever fill in a PBFBuildOptions field the caller
// left at its zero value — an explicit MinZoom/MaxZoom/SimplifyTolerance
// always wins over the preset's value for that field.
//
// Presets are meaningful only for the built-in generator (an empty
// PBFBuildOptions.Generator is implied throughout this package; there is no
// such field here because BuildPBF only ever uses the built-in generator —
// see cmd/tinytiles's separate --generator flag for the external-generator
// CLI path, which presets do not apply to).
type Preset string

const (
	// PresetBalanced reproduces DefaultPBFBuildMinZoom/MaxZoom and the
	// generator's default simplification tolerance, with postal codes off —
	// today's defaults, unchanged. It is also what an empty Preset resolves
	// to.
	PresetBalanced Preset = "balanced"
	// PresetFast trades fidelity for build speed: a narrower zoom range and
	// coarser simplification, for quick local iteration or CI smoke builds.
	PresetFast Preset = "fast"
	// PresetDetailed widens the zoom range and tightens simplification for
	// web maps (MapLibre GL JS, Mapbox GL JS, OpenLayers) that want more
	// fidelity than the default, and suggests enabling the postal_code
	// layer.
	PresetDetailed Preset = "detailed"
	// PresetMobile favors a smaller artifact for offline-first native and
	// browser clients: a narrower zoom range and coarser simplification than
	// PresetBalanced.
	PresetMobile Preset = "mobile"
	// PresetPostcode is tuned for postcode search/reverse-geocode serving
	// and for feeding `tinytiles territory`: otherwise-balanced settings
	// plus a suggestion to enable the postal_code layer.
	PresetPostcode Preset = "postcode"
)

// presetValues holds one preset's concrete generator settings. postalCodes is
// only a suggestion: PBFBuildOptions.PostalCodes is a plain bool with no
// "unset" state, so applying it automatically would make an explicit
// PostalCodes: false indistinguishable from "caller didn't ask for postal
// codes". Callers that want a preset's postal-codes suggestion applied — the
// CLI does this via flagWasSet — must read it from ResolvePreset and decide
// precedence themselves.
type presetValues struct {
	minZoom, maxZoom  int
	simplifyTolerance float64
	postalCodes       bool
}

var presetTable = map[Preset]presetValues{
	PresetBalanced: {minZoom: DefaultPBFBuildMinZoom, maxZoom: DefaultPBFBuildMaxZoom, simplifyTolerance: 4.0, postalCodes: false},
	PresetFast:     {minZoom: 5, maxZoom: 10, simplifyTolerance: 8.0, postalCodes: false},
	PresetDetailed: {minZoom: 5, maxZoom: 16, simplifyTolerance: 2.0, postalCodes: true},
	PresetMobile:   {minZoom: 5, maxZoom: 12, simplifyTolerance: 6.0, postalCodes: false},
	PresetPostcode: {minZoom: 5, maxZoom: 13, simplifyTolerance: 4.0, postalCodes: true},
}

// ResolvePreset returns preset's concrete generator settings. An empty preset
// resolves identically to PresetBalanced. It returns an error for any other
// unrecognized name.
func ResolvePreset(preset Preset) (minZoom, maxZoom int, simplifyTolerance float64, postalCodes bool, err error) {
	if preset == "" {
		preset = PresetBalanced
	}
	values, ok := presetTable[preset]
	if !ok {
		return 0, 0, 0, false, fmt.Errorf("tinytiles: unknown preset %q", preset)
	}
	return values.minZoom, values.maxZoom, values.simplifyTolerance, values.postalCodes, nil
}

// Presets lists every defined preset name, in the same order they are
// documented above.
func Presets() []Preset {
	return []Preset{PresetBalanced, PresetFast, PresetDetailed, PresetMobile, PresetPostcode}
}
