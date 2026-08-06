// Package pmtiles is a read-only PMTiles v3 archive reader used by
// tinyTiles' import path. It is deliberately internal: PMTiles remains an
// input format tinyTiles converts, not a serving artifact or a public
// library surface.
//
// The archive is treated as untrusted input throughout. Every section offset,
// directory size, entry count and tile extent is bounds-checked against the
// file before it is used, and decompression is limited so a malformed archive
// cannot exhaust memory.
package pmtiles

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// HeaderBytes is the fixed PMTiles v3 header size.
	HeaderBytes = 127
	// SpecVersion is the only archive version this reader accepts.
	SpecVersion = 3

	magic = "PMTiles"

	// maxRootDirectoryBytes is the specification's own limit: the header plus
	// the compressed root directory must fit in 16384 bytes.
	maxRootDirectoryBytes = 16384 - HeaderBytes
	// maxLeafDirectoryBytes bounds one compressed leaf directory read.
	maxLeafDirectoryBytes = 32 << 20
	// maxDirectoryBytes bounds a directory after decompression.
	maxDirectoryBytes = 64 << 20
	// maxMetadataBytes bounds the JSON metadata section after decompression.
	maxMetadataBytes = 16 << 20
	// maxTileBytes bounds one tile payload.
	maxTileBytes = 64 << 20
	// maxLeafDepth stops a cyclic or maliciously nested directory tree. The
	// specification discourages more than one level of leaf directories.
	maxLeafDepth = 4
	// maxAddressedTiles bounds a traversal whose header does not declare a
	// tile count. It is far above any real tileset but keeps a corrupt run
	// length from looping indefinitely.
	maxAddressedTiles = 1 << 32
)

// Compression identifies how a section or tile payload is encoded.
type Compression uint8

// PMTiles v3 compression values.
const (
	CompressionUnknown Compression = 0
	CompressionNone    Compression = 1
	CompressionGzip    Compression = 2
	CompressionBrotli  Compression = 3
	CompressionZstd    Compression = 4
)

func (c Compression) String() string {
	switch c {
	case CompressionNone:
		return "none"
	case CompressionGzip:
		return "gzip"
	case CompressionBrotli:
		return "brotli"
	case CompressionZstd:
		return "zstd"
	default:
		return "unknown"
	}
}

// ContentEncoding returns the HTTP content coding for a tile payload stored
// with this compression, or "" when the payload is stored uncompressed.
func (c Compression) ContentEncoding() string {
	switch c {
	case CompressionGzip:
		return "gzip"
	case CompressionBrotli:
		return "br"
	case CompressionZstd:
		return "zstd"
	default:
		return ""
	}
}

// TileType identifies the payload format of every tile in an archive.
type TileType uint8

// PMTiles v3 tile types. Values outside this set are reported as
// TileTypeUnknown so a caller falls back to the archive's JSON metadata
// instead of trusting an unrecognized code.
const (
	TileTypeUnknown TileType = 0
	TileTypeMVT     TileType = 1
	TileTypePNG     TileType = 2
	TileTypeJPEG    TileType = 3
	TileTypeWebP    TileType = 4
	TileTypeAVIF    TileType = 5
)

// MBTilesFormat returns the MBTiles "format" metadata value for this tile
// type, or "" when the archive does not declare a known one.
func (t TileType) MBTilesFormat() string {
	switch t {
	case TileTypeMVT:
		return "pbf"
	case TileTypePNG:
		return "png"
	case TileTypeJPEG:
		return "jpg"
	case TileTypeWebP:
		return "webp"
	case TileTypeAVIF:
		return "avif"
	default:
		return ""
	}
}

// Header is the parsed PMTiles v3 header. Longitude/latitude fields are
// already decoded from their fixed-point E7 representation.
type Header struct {
	RootDirectoryOffset uint64
	RootDirectoryLength uint64
	MetadataOffset      uint64
	MetadataLength      uint64
	LeafDirectoryOffset uint64
	LeafDirectoryLength uint64
	TileDataOffset      uint64
	TileDataLength      uint64

	AddressedTiles uint64
	TileEntries    uint64
	TileContents   uint64

	Clustered           bool
	InternalCompression Compression
	TileCompression     Compression
	TileType            TileType

	MinZoom uint8
	MaxZoom uint8

	MinLongitude, MinLatitude float64
	MaxLongitude, MaxLatitude float64
	CenterZoom                uint8
	CenterLongitude           float64
	CenterLatitude            float64
}

// Archive is an opened PMTiles v3 file.
type Archive struct {
	file   *os.File
	size   int64
	header Header
}

// Tile is one addressed tile. Z/X/Y are XYZ (slippy-map) coordinates, and
// Data is the stored payload exactly as the archive holds it, still encoded
// with Header.TileCompression. Data is only valid for the duration of the
// visiting callback.
type Tile struct {
	Z    uint8
	X, Y uint32
	Data []byte
}

// TileStats describes the expanded tile stream without reading payload data.
// TileBytes counts bytes after expanding run-length entries because a .ttiles
// artifact stores one payload per addressed coordinate in its flat schema.
type TileStats struct {
	Tiles        uint64
	TileBytes    uint64
	MaxTileBytes uint64
}

// TMSRow converts this tile's XYZ row to the MBTiles/TMS row convention.
func (t Tile) TMSRow() uint32 {
	return uint32(1)<<t.Z - 1 - t.Y
}

// Open validates the header of a PMTiles v3 archive. It does not read
// directories or tiles.
func Open(path string) (*Archive, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Size() < HeaderBytes {
		_ = file.Close()
		return nil, fmt.Errorf("pmtiles: %s is %d bytes, shorter than a %d-byte header", path, info.Size(), HeaderBytes)
	}
	archive := &Archive{file: file, size: info.Size()}
	raw := make([]byte, HeaderBytes)
	if _, err := file.ReadAt(raw, 0); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("pmtiles: read header: %w", err)
	}
	if err := archive.header.unmarshal(raw); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := archive.validateSections(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return archive, nil
}

// IsArchive reports whether path begins with a PMTiles v3 header magic. It is
// used to route an import source without relying on its file extension.
func IsArchive(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	raw := make([]byte, len(magic)+1)
	if _, err := io.ReadFull(file, raw); err != nil {
		return false
	}
	return string(raw[:len(magic)]) == magic && raw[len(magic)] == SpecVersion
}

// Header returns the validated archive header.
func (a *Archive) Header() Header { return a.header }

// Close releases the underlying file.
func (a *Archive) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	return a.file.Close()
}

func (h *Header) unmarshal(raw []byte) error {
	if len(raw) < HeaderBytes {
		return fmt.Errorf("pmtiles: header is %d bytes, want %d", len(raw), HeaderBytes)
	}
	if string(raw[:len(magic)]) != magic {
		return errors.New("pmtiles: file does not start with the PMTiles magic number")
	}
	if raw[7] != SpecVersion {
		return fmt.Errorf("pmtiles: archive version %d is unsupported (only v%d)", raw[7], SpecVersion)
	}
	h.RootDirectoryOffset = binary.LittleEndian.Uint64(raw[8:16])
	h.RootDirectoryLength = binary.LittleEndian.Uint64(raw[16:24])
	h.MetadataOffset = binary.LittleEndian.Uint64(raw[24:32])
	h.MetadataLength = binary.LittleEndian.Uint64(raw[32:40])
	h.LeafDirectoryOffset = binary.LittleEndian.Uint64(raw[40:48])
	h.LeafDirectoryLength = binary.LittleEndian.Uint64(raw[48:56])
	h.TileDataOffset = binary.LittleEndian.Uint64(raw[56:64])
	h.TileDataLength = binary.LittleEndian.Uint64(raw[64:72])
	h.AddressedTiles = binary.LittleEndian.Uint64(raw[72:80])
	h.TileEntries = binary.LittleEndian.Uint64(raw[80:88])
	h.TileContents = binary.LittleEndian.Uint64(raw[88:96])
	h.Clustered = raw[96] == 0x01
	h.InternalCompression = Compression(raw[97])
	h.TileCompression = Compression(raw[98])
	h.TileType = TileType(raw[99])
	if h.TileType > TileTypeAVIF {
		h.TileType = TileTypeUnknown
	}
	h.MinZoom = raw[100]
	h.MaxZoom = raw[101]
	h.MinLongitude, h.MinLatitude = decodePosition(raw[102:110])
	h.MaxLongitude, h.MaxLatitude = decodePosition(raw[110:118])
	h.CenterZoom = raw[118]
	h.CenterLongitude, h.CenterLatitude = decodePosition(raw[119:127])
	if h.MinZoom > h.MaxZoom {
		return fmt.Errorf("pmtiles: header min zoom %d exceeds max zoom %d", h.MinZoom, h.MaxZoom)
	}
	if h.MaxZoom > MaxZoom {
		return fmt.Errorf("pmtiles: header max zoom %d is above the supported maximum %d", h.MaxZoom, MaxZoom)
	}
	return nil
}

// decodePosition reads a PMTiles position: little-endian int32 longitude then
// latitude, both scaled by 1e7.
func decodePosition(raw []byte) (longitude, latitude float64) {
	longitude = float64(int32(binary.LittleEndian.Uint32(raw[0:4]))) / 10_000_000
	latitude = float64(int32(binary.LittleEndian.Uint32(raw[4:8]))) / 10_000_000
	return longitude, latitude
}

func (a *Archive) validateSections() error {
	for _, section := range []struct {
		name           string
		offset, length uint64
	}{
		{"root directory", a.header.RootDirectoryOffset, a.header.RootDirectoryLength},
		{"metadata", a.header.MetadataOffset, a.header.MetadataLength},
		{"leaf directories", a.header.LeafDirectoryOffset, a.header.LeafDirectoryLength},
		{"tile data", a.header.TileDataOffset, a.header.TileDataLength},
	} {
		if section.length == 0 {
			continue
		}
		if err := a.checkExtent(section.offset, section.length, section.name); err != nil {
			return err
		}
	}
	if a.header.RootDirectoryLength == 0 {
		return errors.New("pmtiles: archive has an empty root directory")
	}
	if a.header.RootDirectoryLength > maxRootDirectoryBytes {
		return fmt.Errorf("pmtiles: root directory is %d bytes, above the %d-byte specification limit", a.header.RootDirectoryLength, maxRootDirectoryBytes)
	}
	return nil
}

// checkExtent rejects a section that overflows or reaches past end of file.
func (a *Archive) checkExtent(offset, length uint64, name string) error {
	size := uint64(a.size)
	if offset > size || length > size-offset {
		return fmt.Errorf("pmtiles: %s at offset %d length %d extends past the %d-byte file", name, offset, length, a.size)
	}
	return nil
}

func (a *Archive) readSection(offset, length, limit uint64, name string) ([]byte, error) {
	if length > limit {
		return nil, fmt.Errorf("pmtiles: %s is %d bytes, above the %d-byte limit", name, length, limit)
	}
	if err := a.checkExtent(offset, length, name); err != nil {
		return nil, err
	}
	buf := make([]byte, length)
	if _, err := a.file.ReadAt(buf, int64(offset)); err != nil {
		return nil, fmt.Errorf("pmtiles: read %s: %w", name, err)
	}
	return buf, nil
}

// decompress expands a section. brotli and zstd are reported as unsupported
// rather than silently mishandled: neither is in the standard library, and a
// wrong guess would corrupt every tile in the archive.
func decompress(data []byte, compression Compression, limit int64, name string) ([]byte, error) {
	switch compression {
	case CompressionNone:
		return data, nil
	case CompressionGzip:
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("pmtiles: %s gzip header: %w", name, err)
		}
		defer reader.Close()
		out, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return nil, fmt.Errorf("pmtiles: decompress %s: %w", name, err)
		}
		if int64(len(out)) > limit {
			return nil, fmt.Errorf("pmtiles: %s expands above the %d-byte limit", name, limit)
		}
		return out, nil
	case CompressionBrotli, CompressionZstd:
		return nil, fmt.Errorf("pmtiles: %s uses %s compression, which this reader cannot decode", name, compression)
	default:
		return nil, fmt.Errorf("pmtiles: %s uses an unknown compression code %d", name, uint8(compression))
	}
}

// Metadata returns the archive's JSON metadata section, decompressed. It is
// returned as raw bytes because its contents are the producer's, not
// tinyTiles': callers map the documented keys they understand.
func (a *Archive) Metadata() ([]byte, error) {
	if a.header.MetadataLength == 0 {
		return nil, nil
	}
	raw, err := a.readSection(a.header.MetadataOffset, a.header.MetadataLength, maxMetadataBytes, "metadata")
	if err != nil {
		return nil, err
	}
	return decompress(raw, a.header.InternalCompression, maxMetadataBytes, "metadata")
}

type entry struct {
	tileID    uint64
	offset    uint64
	length    uint64
	runLength uint64
}

// deserializeEntries decodes one directory. The four arrays and the
// delta/contiguous encodings follow the PMTiles v3 specification exactly.
func deserializeEntries(buf []byte) ([]entry, error) {
	reader := bytes.NewReader(buf)
	count, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, fmt.Errorf("pmtiles: read directory entry count: %w", err)
	}
	if count == 0 {
		return nil, errors.New("pmtiles: directory declares zero entries")
	}
	// Every entry contributes at least one byte to each of the four arrays,
	// so a count above a quarter of the remaining bytes cannot be honest.
	if count > uint64(reader.Len())/4 {
		return nil, fmt.Errorf("pmtiles: directory declares %d entries but holds only %d bytes", count, reader.Len())
	}
	entries := make([]entry, count)

	var lastID uint64
	for index := range entries {
		delta, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("pmtiles: read tile id: %w", err)
		}
		if delta > ^uint64(0)-lastID {
			return nil, errors.New("pmtiles: tile id overflows")
		}
		lastID += delta
		entries[index].tileID = lastID
	}
	for index := range entries {
		if entries[index].runLength, err = binary.ReadUvarint(reader); err != nil {
			return nil, fmt.Errorf("pmtiles: read run length: %w", err)
		}
	}
	for index := range entries {
		if entries[index].length, err = binary.ReadUvarint(reader); err != nil {
			return nil, fmt.Errorf("pmtiles: read entry length: %w", err)
		}
	}
	for index := range entries {
		value, err := binary.ReadUvarint(reader)
		if err != nil {
			return nil, fmt.Errorf("pmtiles: read entry offset: %w", err)
		}
		if value == 0 && index > 0 {
			previous := entries[index-1]
			if previous.offset > ^uint64(0)-previous.length {
				return nil, errors.New("pmtiles: contiguous entry offset overflows")
			}
			entries[index].offset = previous.offset + previous.length
			continue
		}
		if value == 0 {
			return nil, errors.New("pmtiles: first directory entry uses the contiguous offset encoding")
		}
		entries[index].offset = value - 1
	}
	return entries, nil
}

func (a *Archive) readDirectory(offset, length, limit uint64, name string) ([]entry, error) {
	raw, err := a.readSection(offset, length, limit, name)
	if err != nil {
		return nil, err
	}
	expanded, err := decompress(raw, a.header.InternalCompression, maxDirectoryBytes, name)
	if err != nil {
		return nil, err
	}
	return deserializeEntries(expanded)
}

// EachTile visits every addressed tile in the archive in ascending TileID
// order, expanding run-length encoded entries and following leaf directories.
// A run-length entry names one stored blob shared by several consecutive tile
// identifiers; the blob is read once and reported for each of them.
//
// The callback must not retain Tile.Data beyond its own return.
func (a *Archive) EachTile(ctx context.Context, visit func(Tile) error) error {
	if visit == nil {
		return errors.New("pmtiles: tile visitor is nil")
	}
	root, err := a.readDirectory(a.header.RootDirectoryOffset, a.header.RootDirectoryLength, maxRootDirectoryBytes, "root directory")
	if err != nil {
		return err
	}
	budget := a.header.AddressedTiles
	if budget == 0 {
		budget = maxAddressedTiles
	}
	remaining := budget
	if err := a.walk(ctx, root, 0, &remaining, visit); err != nil {
		return err
	}
	if a.header.AddressedTiles != 0 && remaining != 0 {
		return fmt.Errorf("pmtiles: header declares %d addressed tiles but the directories hold %d", a.header.AddressedTiles, a.header.AddressedTiles-remaining)
	}
	return nil
}

// InspectTiles walks only PMTiles directories and computes exact bounds for a
// subsequent direct artifact import. Tile payload sections are not read.
func (a *Archive) InspectTiles(ctx context.Context) (TileStats, error) {
	root, err := a.readDirectory(a.header.RootDirectoryOffset, a.header.RootDirectoryLength, maxRootDirectoryBytes, "root directory")
	if err != nil {
		return TileStats{}, err
	}
	budget := a.header.AddressedTiles
	if budget == 0 {
		budget = maxAddressedTiles
	}
	remaining := budget
	var stats TileStats
	if err := a.inspectTiles(ctx, root, 0, &remaining, &stats); err != nil {
		return TileStats{}, err
	}
	if a.header.AddressedTiles != 0 && remaining != 0 {
		return TileStats{}, fmt.Errorf("pmtiles: header declares %d addressed tiles but the directories hold %d", a.header.AddressedTiles, a.header.AddressedTiles-remaining)
	}
	return stats, nil
}

func (a *Archive) inspectTiles(ctx context.Context, entries []entry, depth int, remaining *uint64, stats *TileStats) error {
	for _, current := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if current.runLength == 0 {
			if depth >= maxLeafDepth {
				return fmt.Errorf("pmtiles: leaf directories nest deeper than %d levels", maxLeafDepth)
			}
			if current.length > maxLeafDirectoryBytes {
				return fmt.Errorf("pmtiles: leaf directory is %d bytes, above the %d-byte limit", current.length, maxLeafDirectoryBytes)
			}
			offset, err := addOffset(a.header.LeafDirectoryOffset, current.offset)
			if err != nil {
				return err
			}
			leaf, err := a.readDirectory(offset, current.length, maxLeafDirectoryBytes, "leaf directory")
			if err != nil {
				return err
			}
			if err := a.inspectTiles(ctx, leaf, depth+1, remaining, stats); err != nil {
				return err
			}
			continue
		}
		if current.runLength > *remaining {
			return fmt.Errorf("pmtiles: directories address more tiles than the header's %d", a.header.AddressedTiles)
		}
		if current.length > maxTileBytes {
			return fmt.Errorf("pmtiles: tile at id %d is %d bytes, above the %d-byte limit", current.tileID, current.length, maxTileBytes)
		}
		if current.runLength != 0 && current.length > ^uint64(0)/current.runLength {
			return errors.New("pmtiles: expanded tile byte count overflows")
		}
		expandedBytes := current.length * current.runLength
		if stats.TileBytes > ^uint64(0)-expandedBytes || stats.Tiles > ^uint64(0)-current.runLength {
			return errors.New("pmtiles: expanded tile statistics overflow")
		}
		stats.Tiles += current.runLength
		stats.TileBytes += expandedBytes
		if current.length > stats.MaxTileBytes {
			stats.MaxTileBytes = current.length
		}
		*remaining -= current.runLength
	}
	return nil
}

func (a *Archive) walk(ctx context.Context, entries []entry, depth int, remaining *uint64, visit func(Tile) error) error {
	for _, current := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if current.runLength == 0 {
			if depth >= maxLeafDepth {
				return fmt.Errorf("pmtiles: leaf directories nest deeper than %d levels", maxLeafDepth)
			}
			if current.length > maxLeafDirectoryBytes {
				return fmt.Errorf("pmtiles: leaf directory is %d bytes, above the %d-byte limit", current.length, maxLeafDirectoryBytes)
			}
			offset, err := addOffset(a.header.LeafDirectoryOffset, current.offset)
			if err != nil {
				return err
			}
			leaf, err := a.readDirectory(offset, current.length, maxLeafDirectoryBytes, "leaf directory")
			if err != nil {
				return err
			}
			if err := a.walk(ctx, leaf, depth+1, remaining, visit); err != nil {
				return err
			}
			continue
		}
		if current.runLength > *remaining {
			return fmt.Errorf("pmtiles: directories address more tiles than the header's %d", a.header.AddressedTiles)
		}
		if current.length > maxTileBytes {
			return fmt.Errorf("pmtiles: tile at id %d is %d bytes, above the %d-byte limit", current.tileID, current.length, maxTileBytes)
		}
		offset, err := addOffset(a.header.TileDataOffset, current.offset)
		if err != nil {
			return err
		}
		// One stored blob backs the whole run, so read it once.
		data, err := a.readSection(offset, current.length, maxTileBytes, "tile data")
		if err != nil {
			return err
		}
		for step := uint64(0); step < current.runLength; step++ {
			if current.tileID > ^uint64(0)-step {
				return errors.New("pmtiles: run length overflows the tile id space")
			}
			z, x, y, err := IDToZxy(current.tileID + step)
			if err != nil {
				return fmt.Errorf("pmtiles: %w", err)
			}
			if err := visit(Tile{Z: z, X: x, Y: y, Data: data}); err != nil {
				return err
			}
		}
		*remaining -= current.runLength
	}
	return nil
}

func addOffset(base, relative uint64) (uint64, error) {
	if base > ^uint64(0)-relative {
		return 0, errors.New("pmtiles: section offset overflows")
	}
	return base + relative, nil
}
