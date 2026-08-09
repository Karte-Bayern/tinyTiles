//go:build !js && !wasm && !baremetal

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
	"github.com/Karte-Bayern/tinyTiles/v2/internal/territory"
)

// commandTerritory dispatches `tinytiles territory` (build), `tinytiles
// territory validate` and `tinytiles territory inspect`. It needs no SQLite
// build tag: territory building is pure GeoJSON/CSV processing, independent
// of the tile pipeline.
func commandTerritory(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "validate":
			return commandTerritoryValidate(args[1:], stdout, stderr)
		case "inspect":
			return commandTerritoryInspect(args[1:], stdout, stderr)
		}
	}
	return commandTerritoryBuild(args, stdout, stderr)
}

func commandTerritoryBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("territory", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "input GeoJSON FeatureCollection of Polygon/MultiPolygon features")
	field := fs.String("field", "", "GeoJSON property to group by (prefix mode); also the default --geometry-key/--mapping-key")
	group := fs.String("group", "", "grouping rule for prefix mode, e.g. prefix:3")
	mapping := fs.String("mapping", "", "CSV or JSON mapping file joining a geometry key to business attributes")
	geometryKey := fs.String("geometry-key", "", "GeoJSON property to join on (mapping mode); defaults to --field or \"postcode\"")
	mappingKey := fs.String("mapping-key", "", "mapping column to join on (mapping mode); defaults to --geometry-key")
	groupBy := fs.String("group-by", "", "mapping column to group territories by, e.g. territory, employee, vehicle, depot")
	agg := fs.String("agg", "", "comma-separated field:strategy overrides, e.g. employee:first,vehicle:first (strategies: first,unique,list,count,sum,min,max,discard)")
	simplify := fs.String("simplify", "", "simplification tolerance, e.g. 50m or 0.2km")
	preset := fs.String("preset", "", "output preset: powerbi")
	format := fs.String("format", "", "output format override: geojson, topojson or csv (default: inferred from --output extension)")
	output := fs.String("output", "", "output file path")
	replace := fs.Bool("replace", false, "allow replacing an existing --output file")
	if fs.Parse(args) != nil {
		return 2
	}
	if strings.TrimSpace(*input) == "" || strings.TrimSpace(*output) == "" {
		fmt.Fprintln(stderr, "usage: tinytiles territory --input features.geojson [--field F --group prefix:N | --mapping map.csv --group-by COLUMN] --output out.geojson")
		return 2
	}
	if *preset != "" && *preset != "powerbi" {
		fmt.Fprintf(stderr, "tinytiles territory: unknown --preset %q (only \"powerbi\" is defined)\n", *preset)
		return 2
	}
	if !*replace {
		if _, err := os.Stat(*output); err == nil {
			fmt.Fprintf(stderr, "tinytiles territory: output %q already exists; pass --replace to replace it\n", *output)
			return 2
		}
	}

	features, err := readGeoJSONFeatures(*input)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: %v\n", err)
		return 1
	}

	opts, err := resolveTerritoryOptions(features, *field, *group, *mapping, *geometryKey, *mappingKey, *groupBy, *agg)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: %v\n", err)
		return 2
	}

	simplifyMeters, err := territory.ParseDistance(*simplify)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: %v\n", err)
		return 2
	}
	if *preset == "powerbi" && simplifyMeters == 0 {
		simplifyMeters = territory.DefaultPowerBISimplifyMeters
	}
	opts.SimplifyMeters = simplifyMeters

	result, err := territory.Build(opts)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: %v\n", err)
		return 1
	}
	printTerritoryDiagnostics(stdout, result.Diagnostics)

	territories := result.Territories
	if *preset == "powerbi" {
		territories = territory.PowerBIPreset(territories)
	}

	outputFormat, err := resolveTerritoryFormat(*format, *output)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: %v\n", err)
		return 2
	}
	data, err := encodeTerritories(territories, outputFormat)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*output, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "tinytiles territory: write output: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "territories=%d format=%s output=%s\n", len(territories), outputFormat, *output)
	return 0
}

func commandTerritoryValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("territory validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "input GeoJSON FeatureCollection (full validation; omit for a mapping-only check)")
	field := fs.String("field", "", "GeoJSON property to group by (prefix mode)")
	group := fs.String("group", "", "grouping rule for prefix mode, e.g. prefix:3")
	mapping := fs.String("mapping", "", "CSV or JSON mapping file")
	geometryKey := fs.String("geometry-key", "", "GeoJSON property to join on (mapping mode)")
	mappingKey := fs.String("mapping-key", "", "mapping column to join on (mapping mode)")
	groupBy := fs.String("group-by", "", "mapping column to group territories by")
	agg := fs.String("agg", "", "comma-separated field:strategy overrides")
	if fs.Parse(args) != nil {
		return 2
	}

	// `tinytiles territory validate mapping.csv` — a bare mapping file with no
	// --input: check the mapping file's own intrinsic issues only.
	if strings.TrimSpace(*input) == "" && strings.TrimSpace(*mapping) == "" && fs.NArg() == 1 {
		*mapping = fs.Arg(0)
	}
	if strings.TrimSpace(*mapping) == "" && strings.TrimSpace(*input) == "" {
		fmt.Fprintln(stderr, "usage: tinytiles territory validate territories.csv")
		fmt.Fprintln(stderr, "   or: tinytiles territory validate --input features.geojson [--field F --group prefix:N | --mapping map.csv --group-by COLUMN]")
		return 2
	}

	if strings.TrimSpace(*input) == "" {
		table, err := territory.LoadMapping(*mapping)
		if err != nil {
			fmt.Fprintf(stderr, "tinytiles territory validate: %v\n", err)
			return 1
		}
		key := strings.TrimSpace(*mappingKey)
		if key == "" {
			key = "postcode"
		}
		dups := table.DuplicateKeys(key)
		report := map[string]any{
			"rows":                len(table.Rows),
			"columns":             table.Header,
			"key_column":          key,
			"duplicate_key_count": len(dups),
			"duplicate_keys":      dups,
			"ok":                  len(dups) == 0,
		}
		writeJSONReport(stdout, report)
		if len(dups) > 0 {
			return 1
		}
		return 0
	}

	features, err := readGeoJSONFeatures(*input)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory validate: %v\n", err)
		return 1
	}
	opts, err := resolveTerritoryOptions(features, *field, *group, *mapping, *geometryKey, *mappingKey, *groupBy, *agg)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory validate: %v\n", err)
		return 2
	}
	report, err := territory.Validate(opts)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory validate: %v\n", err)
		return 1
	}
	writeJSONReport(stdout, report)
	if !report.OK {
		return 1
	}
	return 0
}

func commandTerritoryInspect(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: tinytiles territory inspect features.geojson")
		return 2
	}
	features, err := readGeoJSONFeatures(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles territory inspect: %v\n", err)
		return 1
	}
	writeJSONReport(stdout, territory.Inspect(features))
	return 0
}

func readGeoJSONFeatures(path string) ([]geo.Feature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read GeoJSON input %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("GeoJSON input %q is empty", path)
	}
	return geo.ReadFeatures(data)
}

// resolveTerritoryOptions turns the CLI's flat flag set into
// territory.Options, choosing prefix mode (--field/--group) or mapping mode
// (--mapping/--group-by) — the two are mutually exclusive.
func resolveTerritoryOptions(features []geo.Feature, field, group, mapping, geometryKey, mappingKey, groupBy, agg string) (territory.Options, error) {
	if group != "" && mapping != "" {
		return territory.Options{}, fmt.Errorf("--group (prefix mode) and --mapping (mapping mode) are mutually exclusive")
	}
	if group == "" && mapping == "" {
		return territory.Options{}, fmt.Errorf("either --group prefix:N or --mapping is required")
	}

	aggSet, err := parseAggregationFlag(agg)
	if err != nil {
		return territory.Options{}, err
	}

	resolvedGeometryKey := strings.TrimSpace(geometryKey)
	if resolvedGeometryKey == "" {
		resolvedGeometryKey = strings.TrimSpace(field)
	}
	if resolvedGeometryKey == "" {
		resolvedGeometryKey = "postcode"
	}

	if group != "" {
		n, err := parsePrefixRule(group)
		if err != nil {
			return territory.Options{}, err
		}
		if strings.TrimSpace(field) == "" && strings.TrimSpace(geometryKey) == "" {
			return territory.Options{}, fmt.Errorf("--group prefix:N requires --field")
		}
		return territory.Options{
			Features:     features,
			GeometryKey:  resolvedGeometryKey,
			PrefixLength: n,
			Aggregation:  aggSet,
		}, nil
	}

	if strings.TrimSpace(groupBy) == "" {
		return territory.Options{}, fmt.Errorf("--mapping requires --group-by")
	}
	table, err := territory.LoadMapping(mapping)
	if err != nil {
		return territory.Options{}, err
	}
	resolvedMappingKey := strings.TrimSpace(mappingKey)
	if resolvedMappingKey == "" {
		resolvedMappingKey = resolvedGeometryKey
	}
	return territory.Options{
		Features:    features,
		GeometryKey: resolvedGeometryKey,
		Mapping:     table,
		MappingKey:  resolvedMappingKey,
		GroupBy:     groupBy,
		Aggregation: aggSet,
	}, nil
}

func parsePrefixRule(group string) (int, error) {
	rest, ok := strings.CutPrefix(group, "prefix:")
	if !ok {
		return 0, fmt.Errorf("--group %q: only \"prefix:N\" is supported", group)
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("--group %q: N must be a positive integer", group)
	}
	return n, nil
}

func parseAggregationFlag(spec string) (territory.AggregationSet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	out := territory.AggregationSet{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		field, strategy, ok := strings.Cut(pair, ":")
		if !ok || strings.TrimSpace(field) == "" || strings.TrimSpace(strategy) == "" {
			return nil, fmt.Errorf("--agg %q: expected field:strategy", pair)
		}
		agg := territory.Aggregation(strings.TrimSpace(strategy))
		switch agg {
		case territory.AggFirst, territory.AggUnique, territory.AggList, territory.AggCount,
			territory.AggSum, territory.AggMin, territory.AggMax, territory.AggDiscard:
		default:
			return nil, fmt.Errorf("--agg %q: unknown strategy %q", pair, strategy)
		}
		out[strings.TrimSpace(field)] = agg
	}
	return out, nil
}

func resolveTerritoryFormat(explicit, output string) (territory.Format, error) {
	if explicit != "" {
		switch territory.Format(explicit) {
		case territory.FormatGeoJSON, territory.FormatTopoJSON, territory.FormatCSV:
			return territory.Format(explicit), nil
		default:
			return "", fmt.Errorf("--format %q: want geojson, topojson or csv", explicit)
		}
	}
	return territory.FormatFromExtension(strings.ToLower(filepath.Ext(output)))
}

func encodeTerritories(territories []territory.Territory, format territory.Format) ([]byte, error) {
	switch format {
	case territory.FormatGeoJSON:
		return territory.ToGeoJSON(territories)
	case territory.FormatTopoJSON:
		return territory.ToTopoJSON(territories)
	case territory.FormatCSV:
		return territory.ToCSV(territories)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func printTerritoryDiagnostics(w io.Writer, d territory.Diagnostics) {
	fmt.Fprintf(w, "unmatched-geometries=%d unmatched-mapping-rows=%d duplicate-mapping-keys=%d empty-group-keys=%d repair-warnings=%d\n",
		len(d.UnmatchedGeometryKeys), len(d.UnmatchedMappingKeys), len(d.DuplicateMappingKeys), d.EmptyGroupKeys, len(d.RepairWarnings))
}

func writeJSONReport(w io.Writer, v any) {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(v)
}
