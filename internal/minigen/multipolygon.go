package minigen

// assembleRings joins way node-ID chains that share an endpoint into closed
// rings — the standard OSM multipolygon reconstruction: a boundary is
// commonly split across several way segments that only close up once
// concatenated. A chain that cannot be closed (missing member, genuinely
// open way) is dropped rather than guessed at.
func assembleRings(chains [][]int64) [][]int64 {
	remaining := make([][]int64, 0, len(chains))
	for _, c := range chains {
		if len(c) >= 2 {
			cp := make([]int64, len(c))
			copy(cp, c)
			remaining = append(remaining, cp)
		}
	}

	var rings [][]int64
	for len(remaining) > 0 {
		current := remaining[0]
		remaining = remaining[1:]
		for current[0] != current[len(current)-1] {
			tail := current[len(current)-1]
			idx, next := findChainFrom(remaining, tail)
			if idx < 0 {
				current = nil // cannot close; discard below
				break
			}
			current = append(current, next...)
			remaining = append(remaining[:idx], remaining[idx+1:]...)
		}
		if len(current) >= 4 && current[0] == current[len(current)-1] {
			rings = append(rings, current)
		}
	}
	return rings
}

// findChainFrom returns the index of a chain in remaining that continues on
// from node tail, and the node IDs to append (its own first node dropped,
// reversed first if tail matched its far end instead of its near end).
func findChainFrom(remaining [][]int64, tail int64) (int, []int64) {
	for i, c := range remaining {
		if c[0] == tail {
			return i, c[1:]
		}
		if c[len(c)-1] == tail {
			return i, reverseNodeIDs(c[:len(c)-1])
		}
	}
	return -1, nil
}

func reverseNodeIDs(ids []int64) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}
