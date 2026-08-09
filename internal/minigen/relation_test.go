package minigen

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestScanPBFRelations(t *testing.T) {
	// table: 0 unused, 1 boundary, 2 postal_code, 3 12345, 4 outer, 5 inner
	strings := message(
		bytesField(1, []byte{}),
		bytesField(1, []byte("boundary")),
		bytesField(1, []byte("postal_code")),
		bytesField(1, []byte("12345")),
		bytesField(1, []byte("outer")),
		bytesField(1, []byte("inner")),
	)
	rel := message(
		varintField(1, 100),
		bytesField(2, packed(1, 2)),
		bytesField(3, packed(2, 3)),
		bytesField(8, packed(4, 5)),
		bytesField(9, packed(zigzagEncode(10), zigzagEncode(1))),
		bytesField(10, packed(1, 1)),
	)
	group := bytesField(4, rel)
	block := message(bytesField(1, strings), bytesField(2, group))
	blob := message(bytesField(1, block))
	header := message(bytesField(1, []byte("OSMData")), varintField(3, uint64(len(blob))))
	file := make([]byte, 4)
	binary.BigEndian.PutUint32(file, uint32(len(header)))
	file = append(file, header...)
	file = append(file, blob...)
	path := filepath.Join(t.TempDir(), "relations.osm.pbf")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}

	var relations []relation
	if err := scanPBFRelations(t.Context(), path, func(r *relation) error {
		relations = append(relations, *r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(relations) != 1 {
		t.Fatalf("relations = %#v", relations)
	}
	r := relations[0]
	if r.ID != 100 {
		t.Errorf("ID = %d, want 100", r.ID)
	}
	if r.Tags["boundary"] != "postal_code" || r.Tags["postal_code"] != "12345" {
		t.Errorf("Tags = %#v", r.Tags)
	}
	want := []relationMember{
		{Type: memberWay, Ref: 10, Role: "outer"},
		{Type: memberWay, Ref: 11, Role: "inner"},
	}
	if len(r.Members) != len(want) || r.Members[0] != want[0] || r.Members[1] != want[1] {
		t.Fatalf("Members = %#v, want %#v", r.Members, want)
	}
}

func TestAssembleRingsJoinsSplitWaySegments(t *testing.T) {
	// A square boundary split into three way segments plus one already-closed
	// standalone triangle in the same batch.
	chains := [][]int64{
		{1, 2, 3},
		{3, 4},
		{4, 1},
		{10, 11, 12, 10},
	}
	rings := assembleRings(chains)
	if len(rings) != 2 {
		t.Fatalf("rings = %#v", rings)
	}
	square := rings[0]
	if len(square) != 5 || square[0] != square[len(square)-1] {
		t.Fatalf("square ring = %v", square)
	}
	seen := map[int64]bool{}
	for _, id := range square[:len(square)-1] {
		seen[id] = true
	}
	for _, want := range []int64{1, 2, 3, 4} {
		if !seen[want] {
			t.Fatalf("square ring %v missing node %d", square, want)
		}
	}
	triangle := rings[1]
	if len(triangle) != 4 || triangle[0] != 10 || triangle[len(triangle)-1] != 10 {
		t.Fatalf("triangle ring = %v", triangle)
	}
}

func TestAssembleRingsJoinsReversedSegment(t *testing.T) {
	// The second segment is stored tail-to-tail with the first and must be
	// reversed before it can extend the chain.
	chains := [][]int64{
		{1, 2, 3},
		{4, 3}, // shares node 3 at its own tail, not its head
		{4, 1},
	}
	rings := assembleRings(chains)
	if len(rings) != 1 {
		t.Fatalf("rings = %#v", rings)
	}
	ring := rings[0]
	if ring[0] != ring[len(ring)-1] {
		t.Fatalf("ring not closed: %v", ring)
	}
}

func TestAssembleRingsDropsUnclosableChain(t *testing.T) {
	chains := [][]int64{
		{1, 2, 3},
		{5, 6}, // unrelated, never closes
	}
	rings := assembleRings(chains)
	if len(rings) != 0 {
		t.Fatalf("expected no closed rings, got %#v", rings)
	}
}
