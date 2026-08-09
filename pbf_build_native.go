//go:build !js && !wasm && !baremetal

package tinytiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
	"github.com/Karte-Bayern/tinyTiles/v2/internal/minigen"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// BuildPBF generates and atomically publishes a .ttiles artifact directly
// from one or more OSM PBF files.
func BuildPBF(ctx context.Context, options PBFBuildOptions) (PBFBuildResult, error) {
	if err := ctx.Err(); err != nil {
		return PBFBuildResult{}, err
	}
	resolved, err := resolvePBFBuildOptions(options)
	if err != nil {
		return PBFBuildResult{}, err
	}
	if err := checkPBFBuildDestination(resolved.ArtifactPath, resolved.ReplaceExisting); err != nil {
		return PBFBuildResult{}, err
	}

	parent := filepath.Dir(resolved.ArtifactPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return PBFBuildResult{}, fmt.Errorf("tinytiles: create PBF build parent: %w", err)
	}
	work, err := os.MkdirTemp(parent, ".tinytiles-pbf-*")
	if err != nil {
		return PBFBuildResult{}, fmt.Errorf("tinytiles: create PBF build workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	emitPBFBuildProgress(resolved.Progress, PBFBuildProgress{Phase: "generate"})
	generated, err := minigen.Build(ctx, minigen.Config{
		PBFInputs:   resolved.PBFInputs,
		Output:      filepath.Join(work, "source.tiles"),
		MinZoom:     resolved.MinZoom,
		MaxZoom:     resolved.MaxZoom,
		Concurrency: resolved.Concurrency,
		PostalCodes: resolved.PostalCodes,
	})
	if err != nil {
		return PBFBuildResult{}, fmt.Errorf("tinytiles: generate PBF tiles: %w", err)
	}
	emitPBFBuildProgress(resolved.Progress, PBFBuildProgress{Phase: "generated"})

	provenance, err := pbfBuildProvenance(resolved)
	if err != nil {
		return PBFBuildResult{}, err
	}
	stream, err := minigen.OpenTileStream(filepath.Join(work, "source.tiles"))
	if err != nil {
		return PBFBuildResult{}, fmt.Errorf("tinytiles: open generated tiles: %w", err)
	}
	imported, err := tiles.ImportTiles(ctx, pbfTileStreamSource{stream: stream}, resolved.ArtifactPath, &tiles.ImportOptions{
		Schema:          resolved.Schema,
		BatchSize:       resolved.BatchSize,
		MaxMemoryBytes:  resolved.MaxMemoryBytes,
		MinFreeBytes:    resolved.MinFreeBytes,
		ReplaceExisting: resolved.ReplaceExisting,
		Provenance:      provenance,
		Progress: func(progress tiles.Progress) {
			copy := progress
			emitPBFBuildProgress(resolved.Progress, PBFBuildProgress{Phase: progress.Phase, Import: &copy})
		},
	})
	if err != nil {
		return PBFBuildResult{}, fmt.Errorf("tinytiles: publish generated tiles: %w", err)
	}

	var postalCodesPath string
	if len(generated.PostalCodes) > 0 {
		postalCodesPath, err = writePostalCodesSidecar(options.ArtifactPath, generated.PostalCodes)
		if err != nil {
			return PBFBuildResult{}, err
		}
	}

	return PBFBuildResult{
		// Preserve the caller's spelling (including a symlinked parent) just
		// like the public tinySQL import API. Internally we use the canonical
		// path above solely for overlap checks and atomic publication.
		ArtifactPath:    options.ArtifactPath,
		Info:            imported.Info,
		Estimate:        imported.Estimate,
		RoadFeatures:    generated.Roads,
		GeneratedTiles:  generated.Tiles,
		PostalCodeCount: len(generated.PostalCodes),
		PostalCodesPath: postalCodesPath,
		Bounds: PBFBounds{
			MinLon: generated.Bounds.MinLon,
			MinLat: generated.Bounds.MinLat,
			MaxLon: generated.Bounds.MaxLon,
			MaxLat: generated.Bounds.MaxLat,
		},
	}, nil
}

// postalCodesSidecarPath derives the sidecar GeoJSON path from an artifact
// path by replacing its final extension (".ttiles", "/", ...) with
// ".postcodes.geojson", e.g. "region.ttiles/" -> "region.postcodes.geojson".
func postalCodesSidecarPath(artifactPath string) string {
	dir, base := filepath.Split(filepath.Clean(artifactPath))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(dir, base+".postcodes.geojson")
}

// writePostalCodesSidecar writes the assembled postal-code boundaries as a
// standalone GeoJSON FeatureCollection next to the published artifact — the
// same format `tinytiles territory --input` reads, so a build with
// PostalCodes enabled can feed straight into territory building.
func writePostalCodesSidecar(artifactPath string, features []minigen.PostalFeature) (string, error) {
	geoFeatures := make([]geo.Feature, len(features))
	for i, f := range features {
		properties := map[string]any{"postcode": f.Code}
		if f.Name != "" {
			properties["name"] = f.Name
		}
		geoFeatures[i] = geo.Feature{Properties: properties, Geometry: f.Geometry}
	}
	data, err := geo.WriteFeatureCollection(geoFeatures)
	if err != nil {
		return "", fmt.Errorf("tinytiles: encode postal code sidecar: %w", err)
	}
	path := postalCodesSidecarPath(artifactPath)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("tinytiles: write postal code sidecar: %w", err)
	}
	return path, nil
}

// pbfTileStreamSource bridges minigen's SQLite-free sequential tile stream to
// tinySQL's native tile-stream importer.
type pbfTileStreamSource struct{ stream *minigen.TileStream }

func (s pbfTileStreamSource) Info(ctx context.Context) (tiles.SourceInfo, error) {
	if err := ctx.Err(); err != nil {
		return tiles.SourceInfo{}, err
	}
	metadata := s.stream.Metadata()
	var count, bytes, maxTileBytes int64
	err := s.stream.Scan(ctx, func(_, _, _ int, data []byte) error {
		count++
		bytes += int64(len(data))
		if size := int64(len(data)); size > maxTileBytes {
			maxTileBytes = size
		}
		return nil
	})
	if err != nil {
		return tiles.SourceInfo{}, fmt.Errorf("tinytiles: scan generated tiles: %w", err)
	}
	name := metadata["name"]
	if name == "" {
		name = "tinyTiles minimal OSM basemap"
	}
	return tiles.SourceInfo{
		Name:         name,
		SourceBytes:  bytes,
		TileCount:    count,
		TileBytes:    bytes,
		MaxTileBytes: maxTileBytes,
		Metadata:     metadata,
	}, nil
}

func (s pbfTileStreamSource) ScanTiles(ctx context.Context, visit func(tiles.Tile) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if visit == nil {
		return errors.New("tinytiles: generated tile visitor is nil")
	}
	return s.stream.Scan(ctx, func(z, x, y int, data []byte) error {
		return visit(tiles.Tile{Key: tiles.Key{Z: z, X: x, Y: y}, Data: data})
	})
}

type resolvedPBFBuildOptions struct {
	PBFBuildOptions
	PBFInputs    []string
	ArtifactPath string
}

func resolvePBFBuildOptions(options PBFBuildOptions) (resolvedPBFBuildOptions, error) {
	if strings.TrimSpace(options.ArtifactPath) == "" {
		return resolvedPBFBuildOptions{}, errors.New("tinytiles: PBF artifact path is required")
	}
	artifactPath, err := canonicalPBFBuildPath(options.ArtifactPath)
	if err != nil {
		return resolvedPBFBuildOptions{}, fmt.Errorf("tinytiles: resolve PBF artifact path: %w", err)
	}
	inputs, err := resolvePBFBuildInputs(options.PBFInputs)
	if err != nil {
		return resolvedPBFBuildOptions{}, err
	}
	for _, input := range inputs {
		if err := rejectPBFBuildPathOverlap(artifactPath, input); err != nil {
			return resolvedPBFBuildOptions{}, err
		}
	}
	if options.MinZoom == 0 && options.MaxZoom == 0 {
		options.MinZoom, options.MaxZoom = DefaultPBFBuildMinZoom, DefaultPBFBuildMaxZoom
	}
	if options.MinZoom < 0 || options.MaxZoom < options.MinZoom || options.MaxZoom > 22 {
		return resolvedPBFBuildOptions{}, fmt.Errorf("tinytiles: invalid PBF zoom range %d..%d", options.MinZoom, options.MaxZoom)
	}
	if options.Concurrency < 0 {
		return resolvedPBFBuildOptions{}, errors.New("tinytiles: PBF concurrency must not be negative")
	}
	if options.BatchSize < 0 {
		return resolvedPBFBuildOptions{}, errors.New("tinytiles: PBF import batch size must not be negative")
	}
	if options.MaxMemoryBytes < 0 {
		return resolvedPBFBuildOptions{}, errors.New("tinytiles: PBF max memory must not be negative")
	}
	if options.MinFreeBytes < 0 {
		return resolvedPBFBuildOptions{}, errors.New("tinytiles: PBF minimum free disk space must not be negative")
	}
	if options.BatchSize == 0 {
		options.BatchSize = DefaultPBFBuildBatchSize
	}
	if options.MaxMemoryBytes == 0 {
		options.MaxMemoryBytes = DefaultPBFBuildMaxMemoryBytes
	}
	if options.MinFreeBytes == 0 {
		options.MinFreeBytes = DefaultPBFBuildMinFreeBytes
	}
	if options.Schema == "" {
		options.Schema = tiles.SchemaAuto
	}
	switch options.Schema {
	case tiles.SchemaAuto, tiles.SchemaFlat, tiles.SchemaNormalized:
	default:
		return resolvedPBFBuildOptions{}, fmt.Errorf("tinytiles: unsupported PBF artifact schema %q", options.Schema)
	}
	return resolvedPBFBuildOptions{PBFBuildOptions: options, PBFInputs: inputs, ArtifactPath: artifactPath}, nil
}

func resolvePBFBuildInputs(inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, errors.New("tinytiles: at least one PBF input is required")
	}
	resolved := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return nil, errors.New("tinytiles: PBF input path is empty")
		}
		info, err := os.Stat(input)
		if err != nil {
			return nil, fmt.Errorf("tinytiles: stat PBF input %q: %w", input, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("tinytiles: PBF input %q is not a regular file", input)
		}
		if info.Size() == 0 {
			return nil, fmt.Errorf("tinytiles: PBF input %q is empty", input)
		}
		path, err := canonicalPBFBuildPath(input)
		if err != nil {
			return nil, fmt.Errorf("tinytiles: resolve PBF input %q: %w", input, err)
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		resolved = append(resolved, path)
	}
	if len(resolved) == 0 {
		return nil, errors.New("tinytiles: at least one PBF input is required")
	}
	return resolved, nil
}

func checkPBFBuildDestination(path string, replace bool) error {
	if _, err := os.Lstat(path); err == nil {
		if !replace {
			return fmt.Errorf("tinytiles: PBF artifact already exists: %s", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tinytiles: inspect PBF artifact target: %w", err)
	}
	return nil
}

func rejectPBFBuildPathOverlap(artifactPath, inputPath string) error {
	if same, err := samePBFBuildPath(artifactPath, inputPath); err != nil {
		return fmt.Errorf("tinytiles: resolve PBF build paths: %w", err)
	} else if same {
		return fmt.Errorf("tinytiles: PBF artifact path %q must not replace PBF input %q", artifactPath, inputPath)
	}
	if within, err := pbfBuildPathWithin(artifactPath, inputPath); err != nil {
		return fmt.Errorf("tinytiles: resolve PBF build paths: %w", err)
	} else if within {
		return fmt.Errorf("tinytiles: PBF artifact path %q must not be inside PBF input %q", artifactPath, inputPath)
	}
	if within, err := pbfBuildPathWithin(inputPath, artifactPath); err != nil {
		return fmt.Errorf("tinytiles: resolve PBF build paths: %w", err)
	} else if within {
		return fmt.Errorf("tinytiles: PBF artifact path %q must not contain PBF input %q", artifactPath, inputPath)
	}
	return nil
}

func samePBFBuildPath(first, second string) (bool, error) {
	firstPath, err := canonicalPBFBuildPath(first)
	if err != nil {
		return false, err
	}
	secondPath, err := canonicalPBFBuildPath(second)
	if err != nil {
		return false, err
	}
	return firstPath == secondPath, nil
}

// canonicalPBFBuildPath resolves symlinks in the path's nearest existing
// parent. This keeps a target below a symlinked directory from bypassing the
// input/output overlap checks before the target itself exists.
func canonicalPBFBuildPath(path string) (string, error) {
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
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if parent := filepath.Dir(candidate); parent == candidate {
			return abs, nil
		}
	}
}

func pbfBuildPathWithin(child, parent string) (bool, error) {
	childPath, err := canonicalPBFBuildPath(child)
	if err != nil {
		return false, err
	}
	parentPath, err := canonicalPBFBuildPath(parent)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(parentPath, childPath)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))), nil
}

func pbfBuildProvenance(options resolvedPBFBuildOptions) (map[string]any, error) {
	inputs := make([]map[string]any, 0, len(options.PBFInputs))
	for _, input := range options.PBFInputs {
		info, err := os.Stat(input)
		if err != nil {
			return nil, fmt.Errorf("tinytiles: stat PBF input %q for provenance: %w", input, err)
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
			"minzoom": options.MinZoom,
			"maxzoom": options.MaxZoom,
			"layers":  []string{"water", "landcover", "building", "transportation"},
		},
	}, nil
}

func emitPBFBuildProgress(callback func(PBFBuildProgress), progress PBFBuildProgress) {
	if callback != nil {
		callback(progress)
	}
}
