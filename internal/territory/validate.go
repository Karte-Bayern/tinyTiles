package territory

import (
	"fmt"
	"sort"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// ComponentReport is one territory's disconnected-component count —
// informational, not an error, since a depot or sales territory may
// legitimately be split across exclaves. It exists so a human (or a
// pipeline diff) can notice an unexpectedly high count and double check the
// mapping that produced it.
type ComponentReport struct {
	ID          string
	Components  int
	MemberCount int
}

// Report is a validate run's full diagnostic output.
type Report struct {
	UnmatchedGeometryKeys []string
	UnmatchedMappingKeys  []string
	DuplicateMappingKeys  []string
	InvalidGeometries     []string
	PossibleOverlaps      []string
	EmptyGroupKeys        int
	Territories           []ComponentReport
	OK                    bool
}

// Validate runs the same join/group/dissolve pipeline as Build and turns its
// diagnostics, plus an overlap check Build itself does not need, into a
// report. It does not fail merely because issues were found — OK reports
// that instead, so a CLI can choose its own exit code.
func Validate(opts Options) (Report, error) {
	result, err := Build(opts)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		UnmatchedGeometryKeys: result.Diagnostics.UnmatchedGeometryKeys,
		UnmatchedMappingKeys:  result.Diagnostics.UnmatchedMappingKeys,
		DuplicateMappingKeys:  result.Diagnostics.DuplicateMappingKeys,
		InvalidGeometries:     result.Diagnostics.RepairWarnings,
		EmptyGroupKeys:        result.Diagnostics.EmptyGroupKeys,
	}
	for _, t := range result.Territories {
		report.Territories = append(report.Territories, ComponentReport{
			ID:          t.ID,
			Components:  len(t.Geometry),
			MemberCount: len(t.Members),
		})
	}
	report.PossibleOverlaps = detectOverlaps(opts)
	report.OK = len(report.UnmatchedGeometryKeys) == 0 &&
		len(report.UnmatchedMappingKeys) == 0 &&
		len(report.DuplicateMappingKeys) == 0 &&
		len(report.InvalidGeometries) == 0 &&
		len(report.PossibleOverlaps) == 0 &&
		report.EmptyGroupKeys == 0
	return report, nil
}

// detectOverlaps flags source-geometry pairs, within the same input set,
// whose bounding boxes intersect in both dimensions (positive width and
// height, not just a shared edge or corner). That is a heuristic, not a
// general polygon intersection test — two oddly shaped, non-convex polygons
// could share a bbox without actually overlapping — but it is deliberately
// robust against the normal, desired case for Dissolve: two polygons that
// only touch along a shared border have a zero-width or zero-height bbox
// intersection (exactly the vertices ray-casting treats ambiguously), so
// they are correctly never flagged.
func detectOverlaps(opts Options) []string {
	features := opts.Features
	type candidate struct {
		index int
		bbox  [4]float64
	}
	candidates := make([]candidate, 0, len(features))
	for i, f := range features {
		if bbox, ok := geo.BBox(f.Geometry); ok {
			candidates = append(candidates, candidate{i, bbox})
		}
	}
	// Sorted minimum longitudes let disjoint candidates stop the inner scan.
	// Dense overlapping inputs still require reporting every matching pair.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].bbox[0] < candidates[j].bbox[0] })
	var warnings []string
	for i, a := range candidates {
		for _, b := range candidates[i+1:] {
			if b.bbox[0] >= a.bbox[2] {
				break
			}
			if !bboxAreaOverlap(a.bbox, b.bbox) {
				continue
			}
			// Preserve the original feature order in diagnostic messages.
			first, second := min(a.index, b.index), max(a.index, b.index)
			left := stringify(features[first].Properties[opts.GeometryKey])
			right := stringify(features[second].Properties[opts.GeometryKey])
			warnings = append(warnings, fmt.Sprintf("%s and %s", left, right))
		}
	}
	return SortedUnique(warnings)
}

func bboxAreaOverlap(a, b [4]float64) bool {
	width := min(a[2], b[2]) - max(a[0], b[0])
	height := min(a[3], b[3]) - max(a[1], b[1])
	return width > 0 && height > 0
}
