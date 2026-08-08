//go:build sqliteimport && !js && !wasm && !baremetal

package minigen

import (
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildWritesStandardMBTilesThroughTinySQL(t *testing.T) {
	block := testPrimitiveBlock()
	blob := message(bytesField(1, block))
	header := message(bytesField(1, []byte("OSMData")), varintField(3, uint64(len(blob))))
	file := make([]byte, 4)
	binary.BigEndian.PutUint32(file, uint32(len(header)))
	file = append(file, header...)
	file = append(file, blob...)
	dir := t.TempDir()
	pbf, mbtiles := filepath.Join(dir, "sample.osm.pbf"), filepath.Join(dir, "sample.mbtiles")
	if err := os.WriteFile(pbf, file, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Build(t.Context(), Config{PBFInputs: []string{pbf}, MBTiles: mbtiles, MinZoom: 14, MaxZoom: 14, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tiles == 0 {
		t.Fatal("Build wrote no tiles")
	}
	db, err := sql.Open("sqlite", mbtiles)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM tiles`).Scan(&count); err != nil || count == 0 {
		t.Fatalf("standard MBTiles tile query: count=%d err=%v", count, err)
	}
}

func testPrimitiveBlock() []byte {
	strings := message(bytesField(1, []byte{}), bytesField(1, []byte("highway")), bytesField(1, []byte("residential")))
	dense := message(bytesField(1, packed(zigzagEncode(10), zigzagEncode(1))), bytesField(8, packed(zigzagEncode(500_000_000), zigzagEncode(10))), bytesField(9, packed(zigzagEncode(80_000_000), zigzagEncode(10))))
	rawWay := message(bytesField(2, packed(1)), bytesField(3, packed(2)), bytesField(8, packed(zigzagEncode(10), zigzagEncode(1))))
	return message(bytesField(1, strings), bytesField(2, message(bytesField(2, dense), bytesField(3, rawWay))))
}
