package territory

import (
	"sort"
	"strconv"
)

// Aggregation names one of the explicit conflict-resolution strategies used
// when a grouped territory's source records disagree on a field's value.
type Aggregation string

const (
	AggFirst   Aggregation = "first"
	AggUnique  Aggregation = "unique"
	AggList    Aggregation = "list"
	AggCount   Aggregation = "count"
	AggSum     Aggregation = "sum"
	AggMin     Aggregation = "min"
	AggMax     Aggregation = "max"
	AggDiscard Aggregation = "discard"
)

// DefaultAggregation applies to any field without an explicit strategy: it
// keeps a scalar when every source record agrees, and falls back to a
// sorted list of the distinct values otherwise — a real conflict stays
// visible in the output instead of being resolved to an arbitrary pick.
const DefaultAggregation = AggUnique

// AggregationSet maps a field name to its configured strategy.
type AggregationSet map[string]Aggregation

func (s AggregationSet) resolve(field string) Aggregation {
	if agg, ok := s[field]; ok {
		return agg
	}
	return DefaultAggregation
}

// Aggregate combines one field's values, in source order, across a
// territory's member records into the value for its output properties. A
// false second result means the field should be omitted entirely.
func Aggregate(agg Aggregation, values []string) (any, bool) {
	if agg == "" {
		agg = DefaultAggregation
	}
	if agg == AggDiscard {
		return nil, false
	}
	nonEmpty := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			nonEmpty = append(nonEmpty, v)
		}
	}
	switch agg {
	case AggFirst:
		if len(nonEmpty) == 0 {
			return nil, false
		}
		return nonEmpty[0], true
	case AggList:
		if len(nonEmpty) == 0 {
			return nil, false
		}
		out := make([]any, len(nonEmpty))
		for i, v := range nonEmpty {
			out[i] = v
		}
		return out, true
	case AggCount:
		return len(SortedUnique(values)), true
	case AggSum, AggMin, AggMax:
		return numericAggregate(agg, nonEmpty)
	case AggUnique:
		fallthrough
	default:
		unique := SortedUnique(values)
		switch len(unique) {
		case 0:
			return nil, false
		case 1:
			return unique[0], true
		default:
			out := make([]any, len(unique))
			for i, v := range unique {
				out[i] = v
			}
			return out, true
		}
	}
}

func numericAggregate(agg Aggregation, values []string) (any, bool) {
	var nums []float64
	for _, v := range values {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return nil, false
	}
	sort.Float64s(nums)
	switch agg {
	case AggSum:
		var sum float64
		for _, n := range nums {
			sum += n
		}
		return sum, true
	case AggMin:
		return nums[0], true
	default: // AggMax
		return nums[len(nums)-1], true
	}
}
