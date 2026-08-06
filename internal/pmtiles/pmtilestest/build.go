// Package pmtilestest builds small PMTiles v3 archives for tests.
//
// It is deliberately a standalone encoder: it shares no code with the reader
// in internal/pmtiles and writes header fields and directory arrays from the
// specification directly. A round-trip test therefore exercises two
// independent implementations rather than confirming one implementation
// against its own assumptions.
package pmtilestest

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"testing"
)

// Compression values from the PMTiles v3 specification.
const (
	CompressionNone   uint8 = 1
	CompressionGzip   uint8 = 2
	CompressionBrotli uint8 = 3
	CompressionZstd   uint8 = 4
)

// Tile type values from the PMTiles v3 specification.
const (
	TileTypeMVT uint8 = 1
	TileTypePNG uint8 = 2
)

// Tile is one directory entry. RunLength zero is written as one; use
// Options.LeafEntries for an explicit leaf-directory pointer instead.
type Tile struct {
	TileID    uint64
	RunLength uint64
	Data      []byte
}

// Options describes the archive to build.
type Options struct {
	InternalCompression uint8
	TileCompression     uint8
	TileType            uint8
	MinZoom, MaxZoom    uint8
	Metadata            string
	Tiles               []Tile
	// UseLeafDirectory moves every tile entry into one leaf directory, leaving
	// the root directory with a single leaf pointer.
	UseLeafDirectory bool
	// AddressedTiles overrides the header's addressed-tile count. Zero writes
	// the true total.
	AddressedTiles uint64
	// Version overrides the header version byte for negative tests.
	Version uint8
	// Magic overrides the magic number for negative tests.
	Magic string
}

// Build writes an archive to path.
func Build(t testing.TB, path string, options Options) {
	t.Helper()
	if err := os.WriteFile(path, Bytes(t, options), 0o644); err != nil {
		t.Fatalf("write pmtiles fixture: %v", err)
	}
}

// Bytes encodes an archive.
func Bytes(t testing.TB, options Options) []byte {
	t.Helper()
	if options.InternalCompression == 0 {
		options.InternalCompression = CompressionNone
	}
	if options.TileCompression == 0 {
		options.TileCompression = CompressionNone
	}
	if options.Version == 0 {
		options.Version = 3
	}
	if options.Magic == "" {
		options.Magic = "PMTiles"
	}

	// Lay out the tile-data section and record each blob's relative offset.
	var tileData bytes.Buffer
	type dirEntry struct{ tileID, offset, length, runLength uint64 }
	entries := make([]dirEntry, 0, len(options.Tiles))
	var addressed uint64
	for _, tile := range options.Tiles {
		runLength := tile.RunLength
		if runLength == 0 {
			runLength = 1
		}
		offset := uint64(tileData.Len())
		tileData.Write(tile.Data)
		entries = append(entries, dirEntry{
			tileID:    tile.TileID,
			offset:    offset,
			length:    uint64(len(tile.Data)),
			runLength: runLength,
		})
		addressed += runLength
	}
	if options.AddressedTiles != 0 {
		addressed = options.AddressedTiles
	}

	serialize := func(list []dirEntry) []byte {
		var buf bytes.Buffer
		scratch := make([]byte, binary.MaxVarintLen64)
		put := func(value uint64) { buf.Write(scratch[:binary.PutUvarint(scratch, value)]) }
		put(uint64(len(list)))
		var last uint64
		for _, item := range list {
			put(item.tileID - last)
			last = item.tileID
		}
		for _, item := range list {
			put(item.runLength)
		}
		for _, item := range list {
			put(item.length)
		}
		var next uint64
		for index, item := range list {
			if index > 0 && item.offset == next {
				put(0)
			} else {
				put(item.offset + 1)
			}
			next = item.offset + item.length
		}
		return buf.Bytes()
	}

	compress := func(raw []byte) []byte {
		if options.InternalCompression != CompressionGzip {
			return raw
		}
		var out bytes.Buffer
		writer := gzip.NewWriter(&out)
		if _, err := writer.Write(raw); err != nil {
			t.Fatalf("gzip fixture section: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close gzip fixture section: %v", err)
		}
		return out.Bytes()
	}

	var rootRaw, leafRaw []byte
	if options.UseLeafDirectory && len(entries) > 0 {
		leafRaw = compress(serialize(entries))
		rootRaw = compress(serialize([]dirEntry{{
			tileID:    entries[0].tileID,
			offset:    0,
			length:    uint64(len(leafRaw)),
			runLength: 0,
		}}))
	} else {
		rootRaw = compress(serialize(entries))
	}
	metadataRaw := compress([]byte(options.Metadata))

	rootOffset := uint64(127)
	metadataOffset := rootOffset + uint64(len(rootRaw))
	leafOffset := metadataOffset + uint64(len(metadataRaw))
	tileOffset := leafOffset + uint64(len(leafRaw))

	header := make([]byte, 127)
	copy(header, options.Magic)
	header[7] = options.Version
	binary.LittleEndian.PutUint64(header[8:16], rootOffset)
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(rootRaw)))
	binary.LittleEndian.PutUint64(header[24:32], metadataOffset)
	binary.LittleEndian.PutUint64(header[32:40], uint64(len(metadataRaw)))
	binary.LittleEndian.PutUint64(header[40:48], leafOffset)
	binary.LittleEndian.PutUint64(header[48:56], uint64(len(leafRaw)))
	binary.LittleEndian.PutUint64(header[56:64], tileOffset)
	binary.LittleEndian.PutUint64(header[64:72], uint64(tileData.Len()))
	binary.LittleEndian.PutUint64(header[72:80], addressed)
	binary.LittleEndian.PutUint64(header[80:88], uint64(len(entries)))
	binary.LittleEndian.PutUint64(header[88:96], uint64(len(entries)))
	header[96] = 0x01
	header[97] = options.InternalCompression
	header[98] = options.TileCompression
	header[99] = options.TileType
	header[100] = options.MinZoom
	header[101] = options.MaxZoom
	// Bounds and center stay zero; the reader does not depend on them.

	var archive bytes.Buffer
	archive.Write(header)
	archive.Write(rootRaw)
	archive.Write(metadataRaw)
	archive.Write(leafRaw)
	archive.Write(tileData.Bytes())
	return archive.Bytes()
}
