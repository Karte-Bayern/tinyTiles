package minigen

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestScanPBFDenseNodesAndWays(t *testing.T) {
	strings := message(bytesField(1, []byte{}), bytesField(1, []byte("highway")), bytesField(1, []byte("residential")))
	dense := message(
		bytesField(1, packed(zigzagEncode(10), zigzagEncode(1))),
		bytesField(8, packed(zigzagEncode(500_000_000), zigzagEncode(10))),
		bytesField(9, packed(zigzagEncode(80_000_000), zigzagEncode(10))),
	)
	rawWay := message(bytesField(2, packed(1)), bytesField(3, packed(2)), bytesField(8, packed(zigzagEncode(10), zigzagEncode(1))))
	group := message(bytesField(2, dense), bytesField(3, rawWay))
	block := message(bytesField(1, strings), bytesField(2, group))
	blob := message(bytesField(1, block))
	header := message(bytesField(1, []byte("OSMData")), varintField(3, uint64(len(blob))))
	file := make([]byte, 4)
	binary.BigEndian.PutUint32(file, uint32(len(header)))
	file = append(file, header...)
	file = append(file, blob...)
	path := filepath.Join(t.TempDir(), "sample.osm.pbf")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	var nodes []node
	var ways []way
	if err := scanPBF(t.Context(), path, 1, func(n *node, w *way) error {
		if n != nil {
			nodes = append(nodes, *n)
		}
		if w != nil {
			ways = append(ways, *w)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != 10 || nodes[1].ID != 11 || nodes[0].Lat != 50 || nodes[1].Lon != 8.000001 {
		t.Fatalf("nodes = %#v", nodes)
	}
	if len(ways) != 1 || ways[0].Tags["highway"] != "residential" || len(ways[0].NodeIDs) != 2 || ways[0].NodeIDs[1] != 11 {
		t.Fatalf("ways = %#v", ways)
	}
}

func message(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}
func bytesField(field int, value []byte) []byte  { return appendMessage(nil, field, value) }
func varintField(field int, value uint64) []byte { return appendVarintField(nil, field, value) }
func packed(values ...uint64) []byte {
	var data []byte
	for _, value := range values {
		data = appendVarint(data, value)
	}
	return data
}
