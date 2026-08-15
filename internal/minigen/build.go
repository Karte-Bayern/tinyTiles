// Package minigen contains tinyTiles' built-in, deliberately small OSM PBF
// generator. It creates a usable compact vector basemap without importing
// code from another project or requiring a second executable.
package minigen

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Config controls the self-contained PBF to tile-stream build. The generator
// intentionally emits transportation, buildings, water and land cover: it is a
// portable offline fallback, not an attempt to duplicate a full cartographic
// product.
type Config struct {
	PBFInputs   []string
	Output      string
	MinZoom     int
	MaxZoom     int
	Concurrency int

	// SimplifyTolerance is a Visvalingam-Whyatt effective-area threshold in
	// squared tile pixels, applied post-projection (see DefaultSimplifyTolerance
	// for the full explanation). Zero or negative selects
	// DefaultSimplifyTolerance.
	SimplifyTolerance float64

	// PostalCodes adds a "postal_code" vector layer assembled from
	// boundary=postal_code relations, and populates Result.PostalCodes. It
	// costs an extra relation/way scan pass, so it is opt-in: a default
	// build never pays for it.
	PostalCodes bool
}

// Result describes the generated source tile stream.
type Result struct {
	Roads       int
	Tiles       int
	Bounds      Bounds
	PostalCodes []PostalFeature
}

// Bounds is the WGS84 extent of the road geometry written to the tileset.
type Bounds struct {
	MinLon, MinLat float64
	MaxLon, MaxLat float64
}

func (b Bounds) String() string {
	return fmt.Sprintf("%.6f,%.6f,%.6f,%.6f", b.MinLon, b.MinLat, b.MaxLon, b.MaxLat)
}

func (b Bounds) Center() (lon, lat float64) {
	return (b.MinLon + b.MaxLon) / 2, (b.MinLat + b.MaxLat) / 2
}

func (b *Bounds) add(point point) {
	if b.MinLon > b.MaxLon {
		b.MinLon, b.MaxLon = point[0], point[0]
		b.MinLat, b.MaxLat = point[1], point[1]
		return
	}
	b.MinLon = min(b.MinLon, point[0])
	b.MinLat = min(b.MinLat, point[1])
	b.MaxLon = max(b.MaxLon, point[0])
	b.MaxLat = max(b.MaxLat, point[1])
}

// Build turns one or more OSM PBF files into a sequential vector tile stream.
// It collects renderable references, loads only their coordinates, then
// decodes and classifies every renderable way exactly once — regardless of
// how many zoom levels are requested — before projecting, simplifying and
// encoding that in-memory feature list once per zoom. Earlier versions
// re-scanned and re-decompressed every input file once per zoom level; that
// full-file work is now paid at most three times total (node refs,
// coordinates, features), however wide the zoom range is.
func Build(ctx context.Context, cfg Config) (Result, error) {
	if len(cfg.PBFInputs) == 0 {
		return Result{}, fmt.Errorf("minigen: at least one PBF input is required")
	}
	if strings.TrimSpace(cfg.Output) == "" {
		return Result{}, fmt.Errorf("minigen: tile stream output is required")
	}
	if cfg.MinZoom < 0 || cfg.MaxZoom < cfg.MinZoom || cfg.MaxZoom > 22 {
		return Result{}, fmt.Errorf("minigen: invalid zoom range %d..%d", cfg.MinZoom, cfg.MaxZoom)
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = runtime.GOMAXPROCS(0)
	}
	if cfg.SimplifyTolerance <= 0 {
		cfg.SimplifyTolerance = DefaultSimplifyTolerance
	}

	refs, err := collectRenderableNodeRefs(ctx, cfg.PBFInputs, cfg.Concurrency)
	if err != nil {
		return Result{}, err
	}
	coords, bounds, err := loadCoordinates(ctx, cfg.PBFInputs, refs, cfg.Concurrency)
	if err != nil {
		return Result{}, err
	}
	if bounds.MinLon > bounds.MaxLon {
		return Result{}, fmt.Errorf("minigen: no renderable map features found in PBF input")
	}

	features, err := collectRenderableFeatures(ctx, cfg.PBFInputs, coords, cfg.Concurrency)
	if err != nil {
		return Result{}, err
	}

	var postalFeatures []PostalFeature
	if cfg.PostalCodes {
		postalFeatures, err = collectPostalFeatures(ctx, cfg.PBFInputs, cfg.Concurrency)
		if err != nil {
			return Result{}, err
		}
	}

	stream, err := createTileStream(cfg.Output, cfg, bounds)
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()

	result := Result{Bounds: bounds, PostalCodes: postalFeatures}
	for zoom := cfg.MinZoom; zoom <= cfg.MaxZoom; zoom++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		builders, roads := buildZoomFromFeatures(features, zoom, cfg.SimplifyTolerance)
		result.Roads += roads
		if len(postalFeatures) > 0 && zoom >= postalMinZoom {
			addPostalFeatures(builders, zoom, postalFeatures, cfg.SimplifyTolerance)
		}
		written, err := writeZoom(stream, builders, cfg.Concurrency)
		if err != nil {
			return Result{}, err
		}
		result.Tiles += written
	}
	if err := stream.Close(); err != nil {
		return Result{}, fmt.Errorf("minigen: finalize tile stream: %w", err)
	}
	return result, nil
}

// scanInputsParallel runs scanOne once per input. With a single input (the
// common case) it runs directly against the caller's ctx, preserving the
// original sequential error-ordering behavior exactly. With more than one
// input, each scanOne call runs in its own goroutine, bounded by concurrency,
// writing to an independent result slot — no shared mutable state exists
// during a scan, so merging happens only after every goroutine finishes. A
// derived context is canceled as soon as any input errors, so sibling scans
// stop promptly instead of running to completion after a failure.
func scanInputsParallel[T any](ctx context.Context, inputs []string, concurrency int, scanOne func(context.Context, string) (T, error)) ([]T, error) {
	if len(inputs) <= 1 {
		out := make([]T, 0, len(inputs))
		for _, input := range inputs {
			v, err := scanOne(ctx, input)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}

	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]T, len(inputs))
	errs := make([]error, len(inputs))
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, input := range inputs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, input string) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i], errs[i] = scanOne(groupCtx, input)
			if errs[i] != nil {
				cancel()
			}
		}(i, input)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func collectRenderableNodeRefs(ctx context.Context, inputs []string, concurrency int) (map[int64]struct{}, error) {
	perInput, err := scanInputsParallel(ctx, inputs, concurrency, func(ctx context.Context, input string) (map[int64]struct{}, error) {
		refs := make(map[int64]struct{})
		err := scanPBF(ctx, input, concurrency, func(node *node, way *way) error {
			if way == nil || !isRenderableWay(way) {
				return nil
			}
			for _, id := range way.NodeIDs {
				refs[id] = struct{}{}
			}
			return nil
		})
		return refs, err
	})
	if err != nil {
		return nil, err
	}
	refs := make(map[int64]struct{})
	for _, m := range perInput {
		for id := range m {
			refs[id] = struct{}{}
		}
	}
	return refs, nil
}

func loadCoordinates(ctx context.Context, inputs []string, refs map[int64]struct{}, concurrency int) (map[int64]point, Bounds, error) {
	type partial struct {
		coords map[int64]point
		bounds Bounds
	}
	perInput, err := scanInputsParallel(ctx, inputs, concurrency, func(ctx context.Context, input string) (partial, error) {
		coords := make(map[int64]point)
		bounds := Bounds{MinLon: math.Inf(1), MinLat: math.Inf(1), MaxLon: math.Inf(-1), MaxLat: math.Inf(-1)}
		err := scanPBF(ctx, input, concurrency, func(node *node, way *way) error {
			if node == nil {
				return nil
			}
			if _, wanted := refs[node.ID]; !wanted {
				return nil
			}
			p := point{node.Lon, node.Lat}
			coords[node.ID] = p
			bounds.add(p)
			return nil
		})
		return partial{coords, bounds}, err
	})
	if err != nil {
		return nil, Bounds{}, err
	}
	coords := make(map[int64]point, len(refs))
	bounds := Bounds{MinLon: math.Inf(1), MinLat: math.Inf(1), MaxLon: math.Inf(-1), MaxLat: math.Inf(-1)}
	for _, part := range perInput {
		for id, p := range part.coords {
			coords[id] = p
		}
		bounds.MinLon = min(bounds.MinLon, part.bounds.MinLon)
		bounds.MinLat = min(bounds.MinLat, part.bounds.MinLat)
		bounds.MaxLon = max(bounds.MaxLon, part.bounds.MaxLon)
		bounds.MaxLat = max(bounds.MaxLat, part.bounds.MaxLat)
	}
	return coords, bounds, nil
}

type tileKey struct{ z, x, y int }

type tileBuilder struct {
	key       tileKey
	layers    map[string][]feature
	tolerance float64
}

// renderableFeature is a way's classification and WGS84 geometry, resolved
// once from its PBF tags and node coordinates. It carries everything a later
// per-zoom pass needs (layer, geometry kind, the zoom threshold it first
// becomes visible at, and its properties) without touching the PBF file
// again.
type renderableFeature struct {
	layer      string
	kind       int
	minZoom    int
	line       []point
	properties map[string]any
}

// collectRenderableFeatures decodes every way exactly once, classifies it via
// roadClass/areaClass, and resolves its line/ring geometry from coords. The
// resulting slice is then reused, in memory, for every zoom in Build's loop
// — replacing what used to be a full PBF re-scan per zoom.
func collectRenderableFeatures(ctx context.Context, inputs []string, coords map[int64]point, concurrency int) ([]renderableFeature, error) {
	perInput, err := scanInputsParallel(ctx, inputs, concurrency, func(ctx context.Context, input string) ([]renderableFeature, error) {
		var features []renderableFeature
		err := scanPBF(ctx, input, concurrency, func(node *node, way *way) error {
			if way == nil {
				return nil
			}
			if class, minZoom, ok := roadClass(way.Tags); ok {
				line := wayLine(coords, way.NodeIDs)
				if len(line) >= 2 {
					features = append(features, renderableFeature{
						layer:      "transportation",
						kind:       geometryLine,
						minZoom:    minZoom,
						line:       line,
						properties: featureProperties(class, way.Tags, true),
					})
				}
				return nil
			}
			layer, class, minZoom, ok := areaClass(way.Tags)
			if !ok || len(way.NodeIDs) < 4 || way.NodeIDs[0] != way.NodeIDs[len(way.NodeIDs)-1] {
				return nil
			}
			ring := wayLine(coords, way.NodeIDs)
			if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
				return nil
			}
			features = append(features, renderableFeature{
				layer:      layer,
				kind:       geometryPolygon,
				minZoom:    minZoom,
				line:       ring,
				properties: featureProperties(class, way.Tags, false),
			})
			return nil
		})
		return features, err
	})
	if err != nil {
		return nil, err
	}
	var all []renderableFeature
	for _, f := range perInput {
		all = append(all, f...)
	}
	return all, nil
}

// buildZoomFromFeatures is the per-zoom step: a pure in-memory filter over
// the already-decoded feature list (no file I/O, no protobuf parsing, no
// zlib) that keeps only features visible at zoom and buckets them into tile
// builders.
func buildZoomFromFeatures(features []renderableFeature, zoom int, tolerance float64) (map[tileKey]*tileBuilder, int) {
	builders := make(map[tileKey]*tileBuilder)
	roads := 0
	for _, f := range features {
		if zoom < f.minZoom {
			continue
		}
		addFeature(builders, zoom, f.layer, feature{kind: f.kind, points: f.line, properties: f.properties}, tolerance)
		if f.kind == geometryLine {
			roads++
		}
	}
	return builders, roads
}

func addFeature(builders map[tileKey]*tileBuilder, zoom int, layer string, f feature, tolerance float64) {
	minLon, minLat, maxLon, maxLat := featureBounds(f)
	x0, y0 := lonLatToTile(minLon, maxLat, zoom)
	x1, y1 := lonLatToTile(maxLon, minLat, zoom)
	// A malformed geometry spanning much of the world would otherwise fan out
	// into an unreasonable number of duplicate tile features.
	if (x1-x0+1)*(y1-y0+1) > 4096 {
		return
	}
	for x := x0; x <= x1; x++ {
		for y := y0; y <= y1; y++ {
			key := tileKey{zoom, x, y}
			builder := builders[key]
			if builder == nil {
				builder = &tileBuilder{key: key, layers: make(map[string][]feature), tolerance: tolerance}
				builders[key] = builder
			}
			builder.layers[layer] = append(builder.layers[layer], f)
		}
	}
}

func featureProperties(class string, tags map[string]string, includeRef bool) map[string]any {
	properties := map[string]any{"class": class}
	if name := strings.TrimSpace(tags["name"]); name != "" {
		properties["name"] = name
	} else if includeRef {
		if ref := strings.TrimSpace(tags["ref"]); ref != "" {
			properties["name"] = ref
		}
	}
	return properties
}

// writeZoom encodes and writes one zoom's tiles in deterministic sorted
// order. Encoding — the CPU-heavy projection/simplify/clip/gzip step — runs
// across a worker pool bounded by concurrency, but every tile is still
// written to the stream sequentially in the same sorted order used before
// concurrency existed here: only the computation parallelizes, never the
// on-disk order or the resulting bytes.
func writeZoom(stream *tileStreamWriter, builders map[tileKey]*tileBuilder, concurrency int) (int, error) {
	keys := make([]tileKey, 0, len(builders))
	for key := range builders {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].z != keys[j].z {
			return keys[i].z < keys[j].z
		}
		if keys[i].x != keys[j].x {
			return keys[i].x < keys[j].x
		}
		return keys[i].y < keys[j].y
	})

	type encodeResult struct {
		data []byte
		err  error
	}
	results := make([]encodeResult, len(keys))
	encodeAt := func(i int) {
		data, err := builders[keys[i]].encode()
		results[i] = encodeResult{data, err}
	}
	if concurrency <= 1 || len(keys) <= 1 {
		for i := range keys {
			encodeAt(i)
		}
	} else {
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i := range keys {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				encodeAt(i)
			}(i)
		}
		wg.Wait()
	}

	written := 0
	for i, key := range keys {
		if results[i].err != nil {
			return 0, results[i].err
		}
		data := results[i].data
		if len(data) == 0 {
			continue
		}
		if err := stream.Write(key.z, key.x, xyzToTMS(key.y, key.z), data); err != nil {
			return 0, err
		}
		written++
	}
	return written, nil
}

func (b *tileBuilder) encode() ([]byte, error) {
	if len(b.layers) == 0 {
		return nil, nil
	}
	return encodeTile(b.key, b.layers, b.tolerance)
}

func tileStreamMetadata(cfg Config, bounds Bounds) map[string]string {
	centerLon, centerLat := bounds.Center()
	vectorLayers := `[{"id":"water","fields":{"class":"String","name":"String"}},{"id":"landcover","fields":{"class":"String","name":"String"}},{"id":"building","fields":{"class":"String","name":"String"}},{"id":"transportation","fields":{"class":"String","name":"String"}}`
	if cfg.PostalCodes {
		vectorLayers += `,{"id":"postal_code","fields":{"class":"String","postal_code":"String","name":"String"}}`
	}
	vectorLayers += `]`
	metadata := map[string]string{
		"name":                "tinyTiles minimal OSM basemap",
		"type":                "baselayer",
		"version":             "1",
		"format":              "pbf",
		"kb:content_encoding": "gzip",
		"minzoom":             fmt.Sprint(cfg.MinZoom),
		"maxzoom":             fmt.Sprint(cfg.MaxZoom),
		"bounds":              bounds.String(),
		"center":              fmt.Sprintf("%.6f,%.6f,%d", centerLon, centerLat, cfg.MinZoom),
		"json":                `{"vector_layers":` + vectorLayers + `}`,
	}
	return metadata
}

func isRenderableWay(way *way) bool {
	if _, _, ok := roadClass(way.Tags); ok {
		return true
	}
	_, _, _, ok := areaClass(way.Tags)
	return ok
}

func areaClass(tags map[string]string) (layer, class string, minZoom int, ok bool) {
	if building := strings.TrimSpace(tags["building"]); building != "" && building != "no" {
		return "building", "building", 14, true
	}
	switch tags["natural"] {
	case "water", "bay", "wetland":
		return "water", "water", 8, true
	case "wood":
		return "landcover", "forest", 9, true
	}
	switch tags["waterway"] {
	case "riverbank":
		return "water", "water", 8, true
	}
	switch tags["landuse"] {
	case "reservoir", "basin":
		return "water", "water", 8, true
	case "forest":
		return "landcover", "forest", 9, true
	case "farmland", "farm", "orchard", "vineyard":
		return "landcover", "farmland", 10, true
	case "meadow":
		return "landcover", "meadow", 10, true
	}
	return "", "", 0, false
}

func roadClass(tags map[string]string) (class string, minZoom int, ok bool) {
	switch tags["highway"] {
	case "motorway", "motorway_link":
		return "motorway", 5, true
	case "trunk", "trunk_link":
		return "trunk", 6, true
	case "primary", "primary_link":
		return "primary", 7, true
	case "secondary", "secondary_link":
		return "secondary", 8, true
	case "tertiary", "tertiary_link":
		return "tertiary", 9, true
	case "unclassified", "residential", "living_street":
		return "residential", 11, true
	case "service", "track":
		return "service", 13, true
	case "path", "footway", "pedestrian", "cycleway", "bridleway", "steps":
		return "path", 14, true
	default:
		return "", 0, false
	}
}

func wayLine(coords map[int64]point, ids []int64) []point {
	line := make([]point, 0, len(ids))
	for _, id := range ids {
		if point, ok := coords[id]; ok {
			line = append(line, point)
		}
	}
	return line
}

// featureBounds computes a feature's WGS84 extent from whichever geometry
// field it uses: the single points ring, or every ring for a multi-ring
// polygon.
func featureBounds(f feature) (minLon, minLat, maxLon, maxLat float64) {
	if len(f.rings) == 0 {
		return lineBounds(f.points)
	}
	minLon, minLat, maxLon, maxLat = math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, r := range f.rings {
		rMinLon, rMinLat, rMaxLon, rMaxLat := lineBounds(r.points)
		minLon, minLat = min(minLon, rMinLon), min(minLat, rMinLat)
		maxLon, maxLat = max(maxLon, rMaxLon), max(maxLat, rMaxLat)
	}
	return
}

func lineBounds(line []point) (minLon, minLat, maxLon, maxLat float64) {
	minLon, minLat, maxLon, maxLat = math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, point := range line {
		minLon, minLat = min(minLon, point[0]), min(minLat, point[1])
		maxLon, maxLat = max(maxLon, point[0]), max(maxLat, point[1])
	}
	return
}

func lonLatToTile(lon, lat float64, zoom int) (x, y int) {
	lat = max(min(lat, 85.05112878), -85.05112878)
	n := math.Exp2(float64(zoom))
	x = int(math.Floor((lon + 180) / 360 * n))
	y = int(math.Floor((1 - math.Log(math.Tan(lat*math.Pi/180)+1/math.Cos(lat*math.Pi/180))/math.Pi) / 2 * n))
	limit := int(n) - 1
	return max(0, min(x, limit)), max(0, min(y, limit))
}

func xyzToTMS(y, zoom int) int { return (1 << zoom) - 1 - y }
