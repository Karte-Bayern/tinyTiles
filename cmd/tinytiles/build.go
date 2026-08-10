//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	tinytiles "github.com/Karte-Bayern/tinyTiles/v2"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// commandBuild provides a self-contained PBF-to-tinyTiles path. Its default
// generator deliberately produces only a compact transportation layer; users
// with a richer, compatible renderer can still select it explicitly.
func commandBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	generator := fs.String("generator", "", "optional compatible PBF→MBTiles generator executable (empty uses built-in minimal generator)")
	mbtilesOut := fs.String("mbtiles-out", "", "persist the generated MBTiles at this path")
	replaceMBTiles := fs.Bool("replace-mbtiles", false, "allow replacement of an existing --mbtiles-out file")
	workDir := fs.String("work-dir", "", "parent directory for temporary MBTiles and shards")
	keepWork := fs.Bool("keep-work", false, "keep temporary source MBTiles and generator files for inspection")
	minZoom := fs.Int("minzoom", 5, "minimum tile zoom")
	maxZoom := fs.Int("maxzoom", 14, "maximum tile zoom")
	postalCodes := fs.Bool("postal-codes", false, "built-in generator only: add a postal_code vector layer from boundary=postal_code relations and write a <dataset-base>.postcodes.geojson sidecar")
	buildingMinZoom := fs.Int("building-minzoom", 12, "external generator only: minimum building zoom")
	shards := fs.Int("shards", 256, "external generator only: temporary generator shard count")
	shardCompression := fs.Bool("shard-compression", true, "external generator only: compress temporary generator shards")
	concurrency := fs.Int("concurrency", 0, "generator decode concurrency; 0 uses its default")
	reduceConcurrency := fs.Int("reduce-concurrency", 0, "external generator only: reduce concurrency")
	districts := fs.String("districts", "", "external generator only: optional district-boundary GeoJSON")
	minLat := fs.Float64("min-lat", 0, "external generator only: southern latitude filter")
	minLon := fs.Float64("min-lon", 0, "external generator only: western longitude filter")
	maxLat := fs.Float64("max-lat", 0, "external generator only: northern latitude filter")
	maxLon := fs.Float64("max-lon", 0, "external generator only: eastern longitude filter")
	centerLat := fs.Float64("center-lat", 0, "external generator only: radius center latitude")
	centerLon := fs.Float64("center-lon", 0, "external generator only: radius center longitude")
	radiusKM := fs.Float64("radius-km", 0, "external generator only: radius in kilometres")
	schema := fs.String("schema", "auto", "artifact schema: auto, flat, normalized")
	compact := fs.Bool("compact", false, "losslessly deduplicate equal tile payloads into a normalized artifact (uses temporary disk)")
	batch := fs.Int("batch", defaultImportBatchSize, "rows per bounded artifact import batch; 0 enables automatic tuning within the memory limit")
	memory := fs.Int64("max-memory", 256<<20, "maximum tinySQL cache budget in bytes")
	reserve := fs.Int64("min-free", 1<<30, "required free disk reserve in bytes")
	replace := fs.Bool("replace", false, "atomically replace an existing artifact")
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: tinytiles build [flags] source.osm.pbf[,more.osm.pbf] dataset.ttiles/")
		return 2
	}

	pbfInputs, err := parsePBFInputs(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
		return 2
	}
	options := externalGeneratorOptions{
		PBFInputs:           pbfInputs,
		MinZoom:             *minZoom,
		MaxZoom:             *maxZoom,
		BuildingMinZoom:     *buildingMinZoom,
		Shards:              *shards,
		ShardCompression:    *shardCompression,
		ShardCompressionSet: true,
		Concurrency:         *concurrency,
		ReduceConcurrency:   *reduceConcurrency,
		Districts:           *districts,
		MinLat:              *minLat,
		MinLon:              *minLon,
		MaxLat:              *maxLat,
		MaxLon:              *maxLon,
		CenterLat:           *centerLat,
		CenterLon:           *centerLon,
		RadiusKM:            *radiusKM,
		CenterLatSet:        flagWasSet(fs, "center-lat"),
		CenterLonSet:        flagWasSet(fs, "center-lon"),
	}
	if err := options.validate(); err != nil {
		fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*districts) != "" {
		if err := requireRegularFile("districts", *districts); err != nil {
			fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
			return 2
		}
	}
	artifactSchema, err := parseSchema(*schema)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *compact && artifactSchema == tiles.SchemaFlat {
		fmt.Fprintln(stderr, "tinytiles build: --compact requires schema auto or normalized; schema flat would discard payload deduplication")
		return 2
	}
	generatorName := strings.TrimSpace(*generator)
	if generatorName == "" {
		// The built-in generator streams tiles directly into the artifact
		// (see BuildPBF); it never produces a real, standalone MBTiles file
		// for --compact to deduplicate or --mbtiles-out to persist.
		if *compact {
			fmt.Fprintln(stderr, "tinytiles build: --compact requires an external --generator; the built-in generator has no MBTiles source to deduplicate")
			return 2
		}
		if *mbtilesOut != "" {
			fmt.Fprintln(stderr, "tinytiles build: --mbtiles-out requires an external --generator; the built-in generator never produces a real MBTiles file")
			return 2
		}
	}
	if *batch < 0 || *memory <= 0 || *reserve < 0 {
		fmt.Fprintln(stderr, "tinytiles build: batch must be zero (automatic) or positive; max-memory must be positive; min-free must not be negative")
		return 2
	}
	if err := validateArtifactBuildPaths(fs.Arg(1), pbfInputs, *districts); err != nil {
		fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
		return 2
	}
	if err := checkArtifactDestination(fs.Arg(1), *replace); err != nil {
		fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
		return 2
	}

	if *mbtilesOut != "" {
		if err := validatePersistentBuildPaths(*mbtilesOut, fs.Arg(1), pbfInputs, *districts); err != nil {
			fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
			return 2
		}
		if err := checkMBTilesDestination(*mbtilesOut, *replaceMBTiles); err != nil {
			fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
			return 2
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if generatorName == "" {
		// The built-in generator delegates to BuildPBF, which streams
		// straight into the artifact through tinySQL's tile-stream importer
		// (tiles.ImportTiles) instead of a real MBTiles database, so none of
		// the work-dir/mbtiles staging below applies to this path. Its
		// Progress callback below reports "phase=generate" itself.
		fmt.Fprintln(stdout, "generator=tinytiles-minimal")
		built, err := tinytiles.BuildPBF(ctx, tinytiles.PBFBuildOptions{
			PBFInputs:       pbfInputs,
			ArtifactPath:    fs.Arg(1),
			MinZoom:         *minZoom,
			MaxZoom:         *maxZoom,
			Concurrency:     *concurrency,
			PostalCodes:     *postalCodes,
			Schema:          artifactSchema,
			BatchSize:       *batch,
			MaxMemoryBytes:  *memory,
			MinFreeBytes:    *reserve,
			ReplaceExisting: *replace,
			Progress: func(progress tinytiles.PBFBuildProgress) {
				fmt.Fprintf(stdout, "phase=%s\n", progress.Phase)
			},
		})
		if err != nil {
			fmt.Fprintf(stderr, "tinytiles build: built-in generation failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "road-features=%d tiles=%d bounds=%.6f,%.6f,%.6f,%.6f\n", built.RoadFeatures, built.GeneratedTiles,
			built.Bounds.MinLon, built.Bounds.MinLat, built.Bounds.MaxLon, built.Bounds.MaxLat)
		fmt.Fprintf(stdout, "artifact=%s schema=%s\n", built.ArtifactPath, built.Info.Schema)
		if built.PostalCodeCount > 0 {
			fmt.Fprintf(stdout, "postal-codes=%d postcodes-geojson=%s\n", built.PostalCodeCount, built.PostalCodesPath)
		}
		return 0
	}
	if *postalCodes {
		fmt.Fprintln(stderr, "tinytiles build: --postal-codes requires the built-in generator (no --generator)")
		return 2
	}

	parent := strings.TrimSpace(*workDir)
	if parent == "" {
		parent = filepath.Dir(filepath.Clean(fs.Arg(1)))
	} else if contained, err := pathWithin(parent, fs.Arg(1)); err != nil {
		fmt.Fprintf(stderr, "tinytiles build: validate work directory: %v\n", err)
		return 2
	} else if contained {
		fmt.Fprintln(stderr, "tinytiles build: work-dir must not be inside the artifact directory")
		return 2
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		fmt.Fprintf(stderr, "tinytiles build: work parent: %v\n", err)
		return 1
	}
	work, err := os.MkdirTemp(parent, ".tinytiles-build-*")
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles build: work directory: %v\n", err)
		return 1
	}
	if *keepWork {
		fmt.Fprintf(stdout, "work-dir=%s\n", work)
	} else {
		defer func() { _ = os.RemoveAll(work) }()
	}

	mbtiles := *mbtilesOut
	if mbtiles == "" {
		mbtiles = filepath.Join(work, "source.mbtiles")
	}
	options.MBTiles = mbtiles
	options.ShardDir = filepath.Join(work, "shards")

	fmt.Fprintln(stdout, "phase=generate")
	generatorPath, lookupErr := exec.LookPath(generatorName)
	if lookupErr != nil {
		fmt.Fprintf(stderr, "tinytiles build: generator %q not found: %v\n", generatorName, lookupErr)
		return 2
	}
	generatorArgs := options.args()
	fmt.Fprintf(stdout, "generator=%s\n", generatorPath)
	fmt.Fprintf(stdout, "generator-args=%s\n", quoteArguments(generatorArgs))
	command := exec.CommandContext(ctx, generatorPath, generatorArgs...)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "tinytiles build: generation cancelled: %v\n", ctx.Err())
		} else {
			fmt.Fprintf(stderr, "tinytiles build: generator failed: %v\n", err)
		}
		return 1
	}
	provenance, err := options.provenance(generatorPath)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles build: collect PBF provenance: %v\n", err)
		return 1
	}
	if err := requireRegularFile("generated MBTiles", mbtiles); err != nil {
		fmt.Fprintf(stderr, "tinytiles build: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "phase=import")
	result, err := importArtifactWithProvenanceAndCompact(ctx, mbtiles, fs.Arg(1), artifactSchema, *batch, *memory, *reserve, *replace, provenance, *compact, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles build: import generated MBTiles: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "artifact=%s schema=%s\n", result.ArtifactPath, result.Info.Schema)
	if *mbtilesOut != "" {
		fmt.Fprintf(stdout, "mbtiles=%s\n", mbtiles)
	}
	return 0
}

func (o externalGeneratorOptions) builtinProvenance() (map[string]any, error) {
	inputs := make([]map[string]any, 0, len(o.PBFInputs))
	for _, input := range o.PBFInputs {
		info, err := os.Stat(input)
		if err != nil {
			return nil, fmt.Errorf("stat PBF input %q: %w", input, err)
		}
		inputs = append(inputs, map[string]any{"name": filepath.Base(input), "bytes": info.Size()})
	}
	return map[string]any{
		"kind":       "osm-pbf",
		"pbf_inputs": inputs,
		"generator": map[string]any{
			"adapter":    "tinytiles-minimal",
			"executable": "builtin",
		},
		"generator_config": map[string]any{
			"minzoom": o.MinZoom,
			"maxzoom": o.MaxZoom,
			"layer":   "transportation",
		},
	}, nil
}

// externalGeneratorOptions contains the stable CLI contract for an optional
// PBF-to-MBTiles generator. The built-in generator deliberately implements only
// the small road-basemap subset; external generators may offer richer styles.
type externalGeneratorOptions struct {
	PBFInputs                                 []string
	MBTiles, ShardDir, Districts              string
	MinZoom, MaxZoom, BuildingMinZoom, Shards int
	ShardCompression, ShardCompressionSet     bool
	Concurrency, ReduceConcurrency            int
	MinLat, MinLon, MaxLat, MaxLon            float64
	CenterLat, CenterLon, RadiusKM            float64
	CenterLatSet, CenterLonSet                bool
}

func (o externalGeneratorOptions) validate() error {
	if len(o.PBFInputs) == 0 {
		return fmt.Errorf("at least one PBF input is required")
	}
	if o.MinZoom < 0 || o.MaxZoom < o.MinZoom || o.MaxZoom > 22 {
		return fmt.Errorf("invalid zoom range: minzoom=%d maxzoom=%d", o.MinZoom, o.MaxZoom)
	}
	if o.BuildingMinZoom < 0 || o.BuildingMinZoom > 22 {
		return fmt.Errorf("invalid building-minzoom=%d", o.BuildingMinZoom)
	}
	if o.Shards < 1 || o.Shards > 4096 {
		return fmt.Errorf("shards must be between 1 and 4096")
	}
	if o.Concurrency < 0 || o.ReduceConcurrency < 0 {
		return fmt.Errorf("concurrency values must not be negative")
	}
	if math.IsNaN(o.RadiusKM) || math.IsInf(o.RadiusKM, 0) || o.RadiusKM < 0 {
		return fmt.Errorf("radius-km must be a finite value >= 0")
	}
	if o.RadiusKM > 0 && (!o.CenterLatSet || !o.CenterLonSet) {
		return fmt.Errorf("radius-km requires both center-lat and center-lon")
	}
	bboxProvided := o.MinLat != 0 || o.MinLon != 0 || o.MaxLat != 0 || o.MaxLon != 0
	if o.RadiusKM > 0 && bboxProvided {
		return fmt.Errorf("cannot combine radius-km with min-lat/min-lon/max-lat/max-lon")
	}
	if bboxProvided {
		if o.MinLat == 0 || o.MinLon == 0 || o.MaxLat == 0 || o.MaxLon == 0 {
			return fmt.Errorf("geographic bounding box requires min-lat, min-lon, max-lat and max-lon")
		}
		if o.MinLat < -90 || o.MinLat > 90 || o.MaxLat < -90 || o.MaxLat > 90 || o.MinLon < -180 || o.MinLon > 180 || o.MaxLon < -180 || o.MaxLon > 180 {
			return fmt.Errorf("geographic bounding box coordinates are out of range")
		}
		if o.MinLat >= o.MaxLat || o.MinLon >= o.MaxLon {
			return fmt.Errorf("geographic bounding box must satisfy min-lat<max-lat and min-lon<max-lon")
		}
	}
	if o.RadiusKM > 0 && (o.CenterLat < -90 || o.CenterLat > 90 || o.CenterLon < -180 || o.CenterLon > 180) {
		return fmt.Errorf("radius center coordinates are out of range")
	}
	return nil
}

func (o externalGeneratorOptions) args() []string {
	args := []string{
		"-pbf", strings.Join(o.PBFInputs, ","),
		"-out", o.MBTiles,
		"-tmp", o.ShardDir,
		"-minzoom", strconv.Itoa(o.MinZoom),
		"-maxzoom", strconv.Itoa(o.MaxZoom),
		"-building-minzoom", strconv.Itoa(o.BuildingMinZoom),
		"-shards", strconv.Itoa(o.Shards),
		"-shard-compression=" + strconv.FormatBool(o.shardCompressionEnabled()),
		"-clean",
		"-districts", o.Districts,
	}
	if o.Concurrency > 0 {
		args = append(args, "-concurrency", strconv.Itoa(o.Concurrency))
	}
	if o.ReduceConcurrency > 0 {
		args = append(args, "-reduce-concurrency", strconv.Itoa(o.ReduceConcurrency))
	}
	if o.MinLat != 0 || o.MinLon != 0 || o.MaxLat != 0 || o.MaxLon != 0 {
		args = append(args,
			"-minLat", strconv.FormatFloat(o.MinLat, 'f', -1, 64),
			"-minLon", strconv.FormatFloat(o.MinLon, 'f', -1, 64),
			"-maxLat", strconv.FormatFloat(o.MaxLat, 'f', -1, 64),
			"-maxLon", strconv.FormatFloat(o.MaxLon, 'f', -1, 64),
		)
	}
	if o.RadiusKM > 0 {
		args = append(args,
			"-centerLat", strconv.FormatFloat(o.CenterLat, 'f', -1, 64),
			"-centerLon", strconv.FormatFloat(o.CenterLon, 'f', -1, 64),
			"-radiusKm", strconv.FormatFloat(o.RadiusKM, 'f', -1, 64),
		)
	}
	return args
}

func (o externalGeneratorOptions) provenance(generatorPath string) (map[string]any, error) {
	inputs := make([]map[string]any, 0, len(o.PBFInputs))
	for _, input := range o.PBFInputs {
		info, err := os.Stat(input)
		if err != nil {
			return nil, fmt.Errorf("stat PBF input %q: %w", input, err)
		}
		inputs = append(inputs, map[string]any{
			"name":  filepath.Base(input),
			"bytes": info.Size(),
		})
	}
	config := map[string]any{
		"minzoom":           o.MinZoom,
		"maxzoom":           o.MaxZoom,
		"building_minzoom":  o.BuildingMinZoom,
		"shards":            o.Shards,
		"shard_compression": o.shardCompressionEnabled(),
	}
	if o.Concurrency > 0 {
		config["concurrency"] = o.Concurrency
	}
	if o.ReduceConcurrency > 0 {
		config["reduce_concurrency"] = o.ReduceConcurrency
	}
	if strings.TrimSpace(o.Districts) != "" {
		config["districts"] = filepath.Base(o.Districts)
	}
	if o.MinLat != 0 || o.MinLon != 0 || o.MaxLat != 0 || o.MaxLon != 0 {
		config["bbox"] = []float64{o.MinLat, o.MinLon, o.MaxLat, o.MaxLon}
	}
	if o.RadiusKM > 0 {
		config["radius"] = map[string]any{"center_lat": o.CenterLat, "center_lon": o.CenterLon, "km": o.RadiusKM}
	}
	return map[string]any{
		"kind":       "osm-pbf",
		"pbf_inputs": inputs,
		"generator": map[string]any{
			"adapter":    "external-generator",
			"executable": filepath.Base(generatorPath),
		},
		"generator_config": config,
	}, nil
}

// shardCompressionEnabled preserves the historical adapter default for the
// zero value used by tests and internal helpers. commandBuild sets the marker
// so an explicit --shard-compression=false is propagated unchanged.
func (o externalGeneratorOptions) shardCompressionEnabled() bool {
	return !o.ShardCompressionSet || o.ShardCompression
}

func parsePBFInputs(raw string) ([]string, error) {
	var inputs []string
	seen := make(map[string]struct{})
	for _, input := range strings.Split(raw, ",") {
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if err := requireRegularFile("PBF input", input); err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(input)
		if err != nil {
			return nil, fmt.Errorf("PBF input %q: %w", input, err)
		}
		absolute = filepath.Clean(absolute)
		if _, duplicate := seen[absolute]; duplicate {
			continue
		}
		seen[absolute] = struct{}{}
		inputs = append(inputs, input)
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("at least one PBF input is required")
	}
	return inputs, nil
}

func requireRegularFile(label, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %q: %w", label, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %q: expected a regular file", label, path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s %q: file is empty", label, path)
	}
	return nil
}

func checkMBTilesDestination(path string, replace bool) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("MBTiles output %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("MBTiles output %q: expected a regular file or a new path", path)
	}
	if !replace {
		return fmt.Errorf("MBTiles output %q already exists; pass --replace-mbtiles to replace it", path)
	}
	return nil
}

// checkArtifactDestination rejects an accidental rebuild over an existing
// artifact before the (potentially long-running) PBF generator starts. The
// importer performs the same check before publication, but doing it here
// avoids wasting a full generation run on an operator mistake.
func checkArtifactDestination(path string, replace bool) error {
	if _, err := os.Lstat(path); err == nil {
		if !replace {
			return fmt.Errorf("artifact %q already exists; pass --replace to replace it", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("artifact target %q: %w", path, err)
	}
	return nil
}

// validateArtifactBuildPaths prevents --replace from turning any build input
// (or a directory that contains one) into the published artifact. Publication
// replaces the target path atomically, so letting either path overlap would
// otherwise delete the input after a successful generation.
func validateArtifactBuildPaths(artifact string, pbfInputs []string, districts string) error {
	for _, input := range pbfInputs {
		if err := rejectOverlappingBuildPaths("artifact", artifact, "PBF input", input); err != nil {
			return err
		}
	}
	if strings.TrimSpace(districts) != "" {
		if err := rejectOverlappingBuildPaths("artifact", artifact, "district input", districts); err != nil {
			return err
		}
	}
	return nil
}

func rejectOverlappingBuildPaths(firstLabel, firstPath, secondLabel, secondPath string) error {
	same, err := samePath(firstPath, secondPath)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("%s %q must not replace %s %q", firstLabel, firstPath, secondLabel, secondPath)
	}
	if within, err := pathWithin(firstPath, secondPath); err != nil {
		return err
	} else if within {
		return fmt.Errorf("%s %q must not be inside %s %q", firstLabel, firstPath, secondLabel, secondPath)
	}
	if within, err := pathWithin(secondPath, firstPath); err != nil {
		return err
	} else if within {
		return fmt.Errorf("%s %q must not contain %s %q", firstLabel, firstPath, secondLabel, secondPath)
	}
	return nil
}

func validatePersistentBuildPaths(mbtiles, artifact string, pbfInputs []string, districts string) error {
	for _, input := range pbfInputs {
		same, err := samePath(mbtiles, input)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("MBTiles output %q must not replace PBF input %q", mbtiles, input)
		}
	}
	if strings.TrimSpace(districts) != "" {
		same, err := samePath(mbtiles, districts)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("MBTiles output %q must not replace district input %q", mbtiles, districts)
		}
	}
	if within, err := pathWithin(mbtiles, artifact); err != nil {
		return err
	} else if within {
		return fmt.Errorf("MBTiles output %q must not be the artifact path or be inside it", mbtiles)
	}
	if within, err := pathWithin(artifact, mbtiles); err != nil {
		return err
	} else if within {
		return fmt.Errorf("artifact path %q must not be inside MBTiles output %q", artifact, mbtiles)
	}
	return nil
}

func samePath(a, b string) (bool, error) {
	aAbs, err := canonicalExistingPath(a)
	if err != nil {
		return false, err
	}
	bAbs, err := canonicalExistingPath(b)
	if err != nil {
		return false, err
	}
	return aAbs == bAbs, nil
}

// canonicalExistingPath resolves symlinks in the path or its nearest existing
// parent. That prevents a not-yet-created output below a symlinked directory
// from evading overlap checks against an existing input.
func canonicalExistingPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	for candidate := abs; ; candidate = filepath.Dir(candidate) {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			relative, relErr := filepath.Rel(candidate, abs)
			if relErr != nil {
				return "", relErr
			}
			return filepath.Clean(filepath.Join(resolved, relative)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if parent := filepath.Dir(candidate); parent == candidate {
			return abs, nil
		}
	}
}

// pathWithin reports whether child is parent itself or is nested inside it.
// Existing paths are canonicalized first so an input reached through a
// symlink cannot evade a check against its real destination. A not-yet-created
// output remains a cleaned absolute path, which is the strongest portable
// comparison possible before its parent creates it.
func pathWithin(child, parent string) (bool, error) {
	childAbs, err := canonicalExistingPath(child)
	if err != nil {
		return false, err
	}
	parentAbs, err := canonicalExistingPath(parent)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(parentAbs, childAbs)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))), nil
}

func quoteArguments(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(current *flag.Flag) {
		if current.Name == name {
			found = true
		}
	})
	return found
}
