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
}

// Result describes the generated source tile stream.
type Result struct {
	Roads  int
	Tiles  int
	Bounds Bounds
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
// It collects renderable references, loads only their coordinates, then streams
// the renderable ways once for every requested zoom. This avoids retaining every
// PBF node while keeping the implementation small.
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

	stream, err := createTileStream(cfg.Output, cfg, bounds)
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()

	result := Result{Bounds: bounds}
	for zoom := cfg.MinZoom; zoom <= cfg.MaxZoom; zoom++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		builders, roads, err := buildZoom(ctx, cfg.PBFInputs, coords, zoom, cfg.Concurrency)
		if err != nil {
			return Result{}, err
		}
		result.Roads += roads
		written, err := writeZoom(stream, builders)
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

func collectRenderableNodeRefs(ctx context.Context, inputs []string, concurrency int) (map[int64]struct{}, error) {
	refs := make(map[int64]struct{})
	for _, input := range inputs {
		err := scanPBF(ctx, input, concurrency, func(node *node, way *way) error {
			if way == nil || !isRenderableWay(way) {
				return nil
			}
			for _, id := range way.NodeIDs {
				refs[id] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func loadCoordinates(ctx context.Context, inputs []string, refs map[int64]struct{}, concurrency int) (map[int64]point, Bounds, error) {
	coords := make(map[int64]point, len(refs))
	bounds := Bounds{MinLon: math.Inf(1), MinLat: math.Inf(1), MaxLon: math.Inf(-1), MaxLat: math.Inf(-1)}
	for _, input := range inputs {
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
		if err != nil {
			return nil, Bounds{}, err
		}
	}
	return coords, bounds, nil
}

type tileKey struct{ z, x, y int }

type tileBuilder struct {
	key    tileKey
	layers map[string][]feature
}

func buildZoom(ctx context.Context, inputs []string, coords map[int64]point, zoom, concurrency int) (map[tileKey]*tileBuilder, int, error) {
	builders := make(map[tileKey]*tileBuilder)
	roads := 0
	for _, input := range inputs {
		err := scanPBF(ctx, input, concurrency, func(node *node, way *way) error {
			if way == nil {
				return nil
			}
			if class, minZoom, ok := roadClass(way.Tags); ok && zoom >= minZoom {
				line := wayLine(coords, way.NodeIDs)
				if len(line) >= 2 {
					feature := feature{kind: geometryLine, points: line, properties: featureProperties(class, way.Tags, true)}
					addFeature(builders, zoom, "transportation", feature)
					roads++
				}
				return nil
			}
			layer, class, minZoom, ok := areaClass(way.Tags)
			if !ok || zoom < minZoom || len(way.NodeIDs) < 4 || way.NodeIDs[0] != way.NodeIDs[len(way.NodeIDs)-1] {
				return nil
			}
			ring := wayLine(coords, way.NodeIDs)
			if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
				return nil
			}
			feature := feature{kind: geometryPolygon, points: ring, properties: featureProperties(class, way.Tags, false)}
			addFeature(builders, zoom, layer, feature)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return builders, roads, nil
}

func addFeature(builders map[tileKey]*tileBuilder, zoom int, layer string, f feature) {
	minLon, minLat, maxLon, maxLat := lineBounds(f.points)
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
				builder = &tileBuilder{key: key, layers: make(map[string][]feature)}
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

func writeZoom(stream *tileStreamWriter, builders map[tileKey]*tileBuilder) (int, error) {
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
	written := 0
	for _, key := range keys {
		data, err := builders[key].encode()
		if err != nil {
			return 0, err
		}
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
	return encodeTile(b.key, b.layers)
}

func tileStreamMetadata(cfg Config, bounds Bounds) map[string]string {
	centerLon, centerLat := bounds.Center()
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
		"json":                `{"vector_layers":[{"id":"water","fields":{"class":"String","name":"String"}},{"id":"landcover","fields":{"class":"String","name":"String"}},{"id":"building","fields":{"class":"String","name":"String"}},{"id":"transportation","fields":{"class":"String","name":"String"}}]}`,
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
