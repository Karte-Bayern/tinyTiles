package minigen

// This file implements the small, read-only subset of the OSM PBF format used
// by the generator. Keeping the wire reader here makes the fallback builder
// self-contained while still accepting normal dense-node OSM extracts.

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const maxPBFBlobSize = 32 << 20

type point [2]float64
type node struct {
	ID       int64
	Lon, Lat float64
}
type way struct {
	ID      int64
	NodeIDs []int64
	Tags    map[string]string
}

func scanPBF(ctx context.Context, path string, _ int, visit func(*node, *way) error) error {
	return scanBlocks(ctx, path, func(data []byte) error {
		return parsePrimitiveBlock(data, visit)
	})
}

// scanBlocks reads every OSMData primitive block out of a PBF file, in
// order, and hands its decompressed payload to fn. It is the shared,
// format-level half of scanning: relation.go reuses it with its own
// relation-focused block parser instead of duplicating the blob-header and
// zlib-unpacking loop.
func scanBlocks(ctx context.Context, path string, fn func(data []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("minigen: open PBF %q: %w", path, err)
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var size [4]byte
		if _, err := io.ReadFull(f, size[:]); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("minigen: read PBF header %q: %w", path, err)
		}
		n := binary.BigEndian.Uint32(size[:])
		if n == 0 || n > 64<<10 {
			return fmt.Errorf("minigen: invalid PBF header size %d", n)
		}
		header := make([]byte, n)
		if _, err := io.ReadFull(f, header); err != nil {
			return fmt.Errorf("minigen: read PBF header: %w", err)
		}
		kind, dataSize, err := parseBlobHeader(header)
		if err != nil {
			return err
		}
		if dataSize < 0 || dataSize > maxPBFBlobSize {
			return fmt.Errorf("minigen: invalid PBF blob size %d", dataSize)
		}
		blob := make([]byte, dataSize)
		if _, err := io.ReadFull(f, blob); err != nil {
			return fmt.Errorf("minigen: read PBF blob: %w", err)
		}
		if kind != "OSMData" {
			continue
		}
		data, err := unpackBlob(blob)
		if err != nil {
			return fmt.Errorf("minigen: decode PBF %q: %w", path, err)
		}
		if err := fn(data); err != nil {
			return fmt.Errorf("minigen: decode PBF %q: %w", path, err)
		}
	}
}

func parseBlobHeader(b []byte) (kind string, dataSize int, err error) {
	dataSize = -1
	return kind, dataSize, walkProto(b, func(num, wire int, value []byte, v uint64) error {
		switch num {
		case 1:
			if wire == 2 {
				kind = string(value)
			}
		case 3:
			if wire == 0 {
				dataSize = int(v)
			}
		}
		return nil
	})
}

func unpackBlob(b []byte) ([]byte, error) {
	var raw, compressed []byte
	if err := walkProto(b, func(num, wire int, value []byte, _ uint64) error {
		if wire != 2 {
			return nil
		}
		if num == 1 {
			raw = value
		}
		if num == 3 {
			compressed = value
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if raw != nil {
		return raw, nil
	}
	if compressed == nil {
		return nil, fmt.Errorf("PBF blob has no supported payload")
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, maxPBFBlobSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPBFBlobSize {
		return nil, fmt.Errorf("uncompressed PBF blob exceeds %d bytes", maxPBFBlobSize)
	}
	return data, nil
}

func parsePrimitiveBlock(b []byte, visit func(*node, *way) error) error {
	var stringsTable []string
	var groups [][]byte
	granularity, latOffset, lonOffset := int64(100), int64(0), int64(0)
	if err := walkProto(b, func(num, wire int, value []byte, v uint64) error {
		switch num {
		case 1:
			if wire == 2 {
				stringsTable = parseStringTable(value)
			}
		case 2:
			if wire == 2 {
				groups = append(groups, value)
			}
		case 17:
			if wire == 0 {
				granularity = int64(v)
			}
		case 19:
			if wire == 0 {
				latOffset = int64(v)
			}
		case 20:
			if wire == 0 {
				lonOffset = int64(v)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, group := range groups {
		if err := parsePrimitiveGroup(group, stringsTable, granularity, latOffset, lonOffset, visit); err != nil {
			return err
		}
	}
	return nil
}

func parseStringTable(b []byte) (out []string) {
	_ = walkProto(b, func(n, w int, v []byte, _ uint64) error {
		if n == 1 && w == 2 {
			out = append(out, string(v))
		}
		return nil
	})
	return
}

func parsePrimitiveGroup(b []byte, table []string, granularity, latOffset, lonOffset int64, visit func(*node, *way) error) error {
	return walkProto(b, func(num, wire int, value []byte, _ uint64) error {
		if wire != 2 {
			return nil
		}
		switch num {
		case 1:
			n, err := parseNode(value, table, granularity, latOffset, lonOffset)
			if err != nil {
				return err
			}
			return visit(n, nil)
		case 2:
			return parseDenseNodes(value, granularity, latOffset, lonOffset, visit)
		case 3:
			w, err := parseWay(value, table)
			if err != nil {
				return err
			}
			return visit(nil, w)
		}
		return nil
	})
}

func parseNode(b []byte, table []string, granularity, latOffset, lonOffset int64) (*node, error) {
	var id, lat, lon int64
	var keys, vals []uint64
	err := walkProto(b, func(n, w int, v []byte, x uint64) error {
		switch n {
		case 1:
			id = zigzag(x)
		case 2:
			keys = append(keys, packedVarints(v)...)
		case 3:
			vals = append(vals, packedVarints(v)...)
		case 8:
			lat = zigzag(x)
		case 9:
			lon = zigzag(x)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = keys
	_ = vals
	return &node{ID: id, Lat: float64(latOffset+granularity*lat) / 1e9, Lon: float64(lonOffset+granularity*lon) / 1e9}, nil
}

func parseDenseNodes(b []byte, granularity, latOffset, lonOffset int64, visit func(*node, *way) error) error {
	var ids, lats, lons []uint64
	if err := walkProto(b, func(n, w int, v []byte, _ uint64) error {
		if w != 2 {
			return nil
		}
		switch n {
		case 1:
			ids = packedVarints(v)
		case 8:
			lats = packedVarints(v)
		case 9:
			lons = packedVarints(v)
		}
		return nil
	}); err != nil {
		return err
	}
	if len(ids) != len(lats) || len(ids) != len(lons) {
		return fmt.Errorf("dense node field lengths differ")
	}
	var id, lat, lon int64
	for i := range ids {
		id += zigzag(ids[i])
		lat += zigzag(lats[i])
		lon += zigzag(lons[i])
		if err := visit(&node{ID: id, Lat: float64(latOffset+granularity*lat) / 1e9, Lon: float64(lonOffset+granularity*lon) / 1e9}, nil); err != nil {
			return err
		}
	}
	return nil
}

func parseWay(b []byte, table []string) (*way, error) {
	w := &way{Tags: map[string]string{}}
	var keys, vals, refs []uint64
	err := walkProto(b, func(n, wire int, v []byte, x uint64) error {
		switch n {
		case 1:
			if wire == 0 {
				w.ID = int64(x) // plain (non-zigzag) int64, unlike a node's sint64 id
			}
		case 2:
			if wire == 2 {
				keys = packedVarints(v)
			}
		case 3:
			if wire == 2 {
				vals = packedVarints(v)
			}
		case 8:
			if wire == 2 {
				refs = packedVarints(v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(keys) && i < len(vals); i++ {
		k, v := int(keys[i]), int(vals[i])
		if k < len(table) && v < len(table) {
			w.Tags[table[k]] = table[v]
		}
	}
	var id int64
	for _, ref := range refs {
		id += zigzag(ref)
		w.NodeIDs = append(w.NodeIDs, id)
	}
	return w, nil
}

func walkProto(b []byte, fn func(num, wire int, value []byte, v uint64) error) error {
	for len(b) > 0 {
		key, n := readVarint(b)
		if n <= 0 {
			return fmt.Errorf("invalid protobuf key")
		}
		b = b[n:]
		num, wire := int(key>>3), int(key&7)
		var value []byte
		var v uint64
		switch wire {
		case 0:
			v, n = readVarint(b)
			if n <= 0 {
				return fmt.Errorf("invalid protobuf varint")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return fmt.Errorf("truncated protobuf fixed64")
			}
			value = b[:8]
			b = b[8:]
		case 2:
			var size uint64
			size, n = readVarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				return fmt.Errorf("invalid protobuf length")
			}
			b = b[n:]
			value = b[:size]
			b = b[size:]
		case 5:
			if len(b) < 4 {
				return fmt.Errorf("truncated protobuf fixed32")
			}
			value = b[:4]
			b = b[4:]
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
		if err := fn(num, wire, value, v); err != nil {
			return err
		}
	}
	return nil
}
func readVarint(b []byte) (uint64, int) {
	var v uint64
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		v |= uint64(c&127) << uint(7*i)
		if c < 128 {
			return v, i + 1
		}
	}
	return 0, 0
}
func packedVarints(b []byte) (out []uint64) {
	for len(b) > 0 {
		v, n := readVarint(b)
		if n <= 0 {
			return nil
		}
		out = append(out, v)
		b = b[n:]
	}
	return
}
func zigzag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }
