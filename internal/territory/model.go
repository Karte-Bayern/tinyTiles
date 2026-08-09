// Package territory turns polygon geometries — postal codes, administrative
// boundaries, or any other polygon dataset — into custom business
// territories: sales regions, field-service areas, delivery zones. It is
// deliberately generic: grouping, dissolve and export never assume the
// member geometries are postal codes, only that they carry a joinable key
// property.
package territory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// Territory is one dissolved group of member geometries plus the business
// metadata attached to it. Members are not assumed to be postal codes, or
// even geographically contiguous — Geometry is a MultiPolygon precisely so a
// territory made of disconnected parts (an exclave depot area, a split sales
// region) is represented correctly instead of merged into a false single
// shape.
type Territory struct {
	ID         string
	Name       string
	Geometry   geo.MultiPolygon
	Properties map[string]any
	Members    []string
}

// Member is one input geometry feature carried into the grouping stage: its
// join-key value, its geometry, and whichever mapping/source properties are
// available for aggregation.
type Member struct {
	Key        string
	Geometry   geo.MultiPolygon
	Properties map[string]string
}

// stringify renders a GeoJSON property value as a plain string for grouping,
// joins and aggregation — numbers lose a trailing ".0" so "84130" and
// 84130.0 compare equal.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// PrefixGroup returns the first n runes of value, tinyTiles' one built-in
// convenience grouping rule (postcode → prefix:1/2/3 → territory). Shorter
// values are returned unchanged rather than padded, since a truncated
// group key would silently merge unrelated territories.
func PrefixGroup(value string, n int) string {
	runes := []rune(value)
	if n <= 0 || n >= len(runes) {
		return value
	}
	return string(runes[:n])
}

// SortedUnique returns the distinct, sorted, non-empty values of values.
func SortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns the keys of m in sorted order, for deterministic
// iteration anywhere grouping uses a Go map.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalizeKey trims and lower-cases a join or grouping key so incidental
// case/whitespace differences between a GeoJSON property and a mapping
// file's column do not silently produce unmatched rows.
func normalizeKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
