package territory

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// Options controls one territory build. Exactly one grouping mode applies:
// Mapping set selects the mapping-table join (--mapping/--geometry-key/
// --mapping-key/--group-by); Mapping nil selects prefix grouping
// (--field/--group prefix:N) directly off GeometryKey's value.
type Options struct {
	Features    []geo.Feature
	GeometryKey string

	// PrefixLength is the "prefix:N" length used when Mapping is nil. Zero
	// groups by each feature's full, unmodified GeometryKey value.
	PrefixLength int

	Mapping    *MappingTable
	MappingKey string
	GroupBy    string

	Aggregation    AggregationSet
	SimplifyMeters float64
}

// Diagnostics reports join and repair issues found while building — surfaced
// by both the main build command (as warnings) and `territory validate` (as
// the report).
type Diagnostics struct {
	UnmatchedGeometryKeys []string // geometry key values with no mapping row
	UnmatchedMappingKeys  []string // mapping key values with no geometry
	DuplicateMappingKeys  []string
	RepairWarnings        []string
	EmptyGroupKeys        int // features whose group key was blank, skipped
}

// Result is one completed territory build.
type Result struct {
	Territories []Territory
	Diagnostics Diagnostics
}

// Build joins, groups, dissolves and aggregates opts.Features into
// Territories. It never fails on a single bad input feature or an unmatched
// join key — those are reported through Diagnostics so a partial dataset
// still produces output, matching a scriptable-pipeline tool's usual
// "warn and continue" posture; only a structurally invalid Options
// (missing required fields) returns an error.
func Build(opts Options) (Result, error) {
	if strings.TrimSpace(opts.GeometryKey) == "" {
		return Result{}, fmt.Errorf("territory: geometry key field is required")
	}
	if opts.Mapping != nil && strings.TrimSpace(opts.GroupBy) == "" {
		return Result{}, fmt.Errorf("territory: --group-by is required with --mapping")
	}

	var diagnostics Diagnostics
	var mappingIndex map[string][]MappingRow
	usedMappingKeys := map[string]bool{}
	if opts.Mapping != nil {
		mappingIndex = opts.Mapping.Index(opts.MappingKey)
		diagnostics.DuplicateMappingKeys = opts.Mapping.DuplicateKeys(opts.MappingKey)
	}

	groups := map[string][]Member{}
	for _, feature := range opts.Features {
		rawKey := stringify(feature.Properties[opts.GeometryKey])
		if rawKey == "" {
			diagnostics.EmptyGroupKeys++
			continue
		}
		repaired, warnings := geo.Repair(feature.Geometry, rawKey)
		diagnostics.RepairWarnings = append(diagnostics.RepairWarnings, warnings...)
		if len(repaired) == 0 {
			continue
		}

		if opts.Mapping == nil {
			groupKey := rawKey
			if opts.PrefixLength > 0 {
				groupKey = PrefixGroup(rawKey, opts.PrefixLength)
			}
			groups[groupKey] = append(groups[groupKey], Member{
				Key:        rawKey,
				Geometry:   repaired,
				Properties: stringifyProperties(feature.Properties, opts.GeometryKey),
			})
			continue
		}

		rows, matched := mappingIndex[normalizeKey(rawKey)]
		if !matched {
			diagnostics.UnmatchedGeometryKeys = append(diagnostics.UnmatchedGeometryKeys, rawKey)
			continue
		}
		usedMappingKeys[normalizeKey(rawKey)] = true
		for _, row := range rows {
			groupKey := strings.TrimSpace(row[opts.GroupBy])
			if groupKey == "" {
				diagnostics.EmptyGroupKeys++
				continue
			}
			props := stringifyProperties(feature.Properties, opts.GeometryKey)
			for k, v := range row {
				if k == opts.GroupBy || k == opts.MappingKey {
					continue // already captured via source_values/Members
				}
				props[k] = v
			}
			groups[groupKey] = append(groups[groupKey], Member{
				Key:        rawKey,
				Geometry:   repaired,
				Properties: props,
			})
		}
	}

	if opts.Mapping != nil {
		for key := range mappingIndex {
			if !usedMappingKeys[key] {
				diagnostics.UnmatchedMappingKeys = append(diagnostics.UnmatchedMappingKeys, key)
			}
		}
		diagnostics.UnmatchedGeometryKeys = SortedUnique(diagnostics.UnmatchedGeometryKeys)
		diagnostics.UnmatchedMappingKeys = SortedUnique(diagnostics.UnmatchedMappingKeys)
	}

	territories := make([]Territory, 0, len(groups))
	for _, groupKey := range sortedKeys(groups) {
		members := groups[groupKey]
		t, err := buildTerritory(groupKey, members, opts)
		if err != nil {
			return Result{}, fmt.Errorf("territory %q: %w", groupKey, err)
		}
		territories = append(territories, t)
	}
	return Result{Territories: territories, Diagnostics: diagnostics}, nil
}

func buildTerritory(groupKey string, members []Member, opts Options) (Territory, error) {
	geometries := make([]geo.MultiPolygon, len(members))
	sourceKeys := make([]string, len(members))
	fieldValues := map[string][]string{}
	for i, m := range members {
		geometries[i] = m.Geometry
		sourceKeys[i] = m.Key
		for field, value := range m.Properties {
			fieldValues[field] = append(fieldValues[field], value)
		}
	}

	dissolved, err := geo.Dissolve(geometries)
	if err != nil {
		return Territory{}, err
	}
	if opts.SimplifyMeters > 0 {
		dissolved = geo.Simplify(dissolved, opts.SimplifyMeters)
	}

	props := map[string]any{
		"territory_id":         groupKey,
		"territory_name":       groupKey,
		"source_feature_count": len(members),
		"source_values":        toAnySlice(SortedUnique(sourceKeys)),
	}
	props["area_km2"] = roundTo(geo.AreaKM2(dissolved), 4)
	for _, field := range sortedKeys(fieldValues) {
		agg := opts.Aggregation.resolve(field)
		if value, ok := Aggregate(agg, fieldValues[field]); ok {
			props[field] = value
		}
	}

	return Territory{
		ID:         groupKey,
		Name:       groupKey,
		Geometry:   dissolved,
		Properties: props,
		Members:    SortedUnique(sourceKeys),
	}, nil
}

func stringifyProperties(properties map[string]any, exclude string) map[string]string {
	out := make(map[string]string, len(properties))
	for k, v := range properties {
		if k == exclude {
			continue
		}
		out[k] = stringify(v)
	}
	return out
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func roundTo(v float64, decimals int) float64 {
	scale := 1.0
	for i := 0; i < decimals; i++ {
		scale *= 10
	}
	return float64(int64(v*scale+sign(v)*0.5)) / scale
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// ParseDistance parses a simplification tolerance like "50m" or "0.2km" into
// meters. A bare number (no suffix) is treated as meters.
func ParseDistance(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	unit := 1.0
	switch {
	case strings.HasSuffix(s, "km"):
		unit = 1000
		s = strings.TrimSuffix(s, "km")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("territory: invalid distance %q (want e.g. \"50m\" or \"0.2km\"): %w", s, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("territory: distance %q must not be negative", s)
	}
	return value * unit, nil
}
