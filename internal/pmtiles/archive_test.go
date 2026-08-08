package pmtiles

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/pmtiles/pmtilestest"
)

func buildArchive(t *testing.T, options pmtilestest.Options) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.pmtiles")
	pmtilestest.Build(t, path, options)
	return path
}

func collectTiles(t *testing.T, path string) []Tile {
	t.Helper()
	archive, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer archive.Close()
	var collected []Tile
	if err := archive.EachTile(context.Background(), func(tile Tile) error {
		collected = append(collected, Tile{Z: tile.Z, X: tile.X, Y: tile.Y, Data: append([]byte(nil), tile.Data...)})
		return nil
	}); err != nil {
		t.Fatalf("EachTile: %v", err)
	}
	return collected
}

func TestArchiveReadsTilesAndMetadata(t *testing.T) {
	t.Parallel()
	for _, compression := range []struct {
		name  string
		value uint8
	}{
		{"uncompressed", pmtilestest.CompressionNone},
		{"gzip", pmtilestest.CompressionGzip},
	} {
		t.Run(compression.name, func(t *testing.T) {
			t.Parallel()
			path := buildArchive(t, pmtilestest.Options{
				InternalCompression: compression.value,
				TileType:            pmtilestest.TileTypeMVT,
				MaxZoom:             1,
				Metadata:            `{"name":"fixture","vector_layers":[{"id":"roads"}]}`,
				Tiles: []pmtilestest.Tile{
					{TileID: ZxyToID(0, 0, 0), Data: []byte("zero")},
					{TileID: ZxyToID(1, 0, 1), Data: []byte("one-zero-one")},
				},
			})
			archive, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer archive.Close()

			header := archive.Header()
			if header.TileType != TileTypeMVT || header.MaxZoom != 1 {
				t.Fatalf("header = %+v", header)
			}
			if got := header.TileType.MBTilesFormat(); got != "pbf" {
				t.Fatalf("MBTilesFormat = %q, want pbf", got)
			}
			metadata, err := archive.Metadata()
			if err != nil {
				t.Fatalf("Metadata: %v", err)
			}
			if !strings.Contains(string(metadata), `"vector_layers"`) {
				t.Fatalf("metadata = %s", metadata)
			}

			tiles := collectTiles(t, path)
			if len(tiles) != 2 {
				t.Fatalf("read %d tiles, want 2", len(tiles))
			}
			if tiles[0].Z != 0 || string(tiles[0].Data) != "zero" {
				t.Fatalf("tile 0 = %+v", tiles[0])
			}
			if tiles[1].Z != 1 || tiles[1].X != 0 || tiles[1].Y != 1 || string(tiles[1].Data) != "one-zero-one" {
				t.Fatalf("tile 1 = %+v", tiles[1])
			}
			// XYZ row 1 at zoom 1 is TMS row 0.
			if got := tiles[1].TMSRow(); got != 0 {
				t.Fatalf("TMSRow = %d, want 0", got)
			}
		})
	}
}

// TestArchiveExpandsRunLengthEntries covers the encoding PMTiles uses to
// deduplicate repeated tiles: one stored blob addressed by several
// consecutive identifiers. Every one of them must be reported.
func TestArchiveExpandsRunLengthEntries(t *testing.T) {
	t.Parallel()
	path := buildArchive(t, pmtilestest.Options{
		TileType: pmtilestest.TileTypePNG,
		MaxZoom:  1,
		Tiles: []pmtilestest.Tile{
			{TileID: ZxyToID(1, 0, 0), RunLength: 4, Data: []byte("ocean")},
		},
	})
	tiles := collectTiles(t, path)
	if len(tiles) != 4 {
		t.Fatalf("read %d tiles, want the run expanded to 4", len(tiles))
	}
	seen := map[string]bool{}
	for _, tile := range tiles {
		if string(tile.Data) != "ocean" {
			t.Fatalf("tile %+v does not carry the shared payload", tile)
		}
		if tile.Z != 1 {
			t.Fatalf("tile %+v left zoom 1", tile)
		}
		key := string(rune(tile.X)) + ":" + string(rune(tile.Y))
		if seen[key] {
			t.Fatalf("coordinate %d/%d repeated", tile.X, tile.Y)
		}
		seen[key] = true
	}
	if len(seen) != 4 {
		t.Fatalf("run produced %d distinct coordinates, want 4", len(seen))
	}
}

func TestArchiveFollowsLeafDirectories(t *testing.T) {
	t.Parallel()
	path := buildArchive(t, pmtilestest.Options{
		InternalCompression: pmtilestest.CompressionGzip,
		TileType:            pmtilestest.TileTypeMVT,
		MaxZoom:             2,
		UseLeafDirectory:    true,
		Tiles: []pmtilestest.Tile{
			{TileID: ZxyToID(2, 0, 0), Data: []byte("a")},
			{TileID: ZxyToID(2, 1, 1), Data: []byte("b")},
			{TileID: ZxyToID(2, 3, 3), Data: []byte("c")},
		},
	})
	tiles := collectTiles(t, path)
	if len(tiles) != 3 {
		t.Fatalf("read %d tiles through the leaf directory, want 3", len(tiles))
	}
	payloads := ""
	for _, tile := range tiles {
		if tile.Z != 2 {
			t.Fatalf("tile %+v left zoom 2", tile)
		}
		payloads += string(tile.Data)
	}
	if payloads != "abc" {
		t.Fatalf("leaf payloads = %q, want abc in tile-id order", payloads)
	}
}

func TestIsArchiveDetectsByHeaderNotExtension(t *testing.T) {
	t.Parallel()
	path := buildArchive(t, pmtilestest.Options{
		TileType: pmtilestest.TileTypeMVT,
		Tiles:    []pmtilestest.Tile{{TileID: 0, Data: []byte("x")}},
	})
	renamed := filepath.Join(t.TempDir(), "archive.mbtiles")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renamed, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsArchive(renamed) {
		t.Fatal("PMTiles archive with an .mbtiles name was not detected")
	}

	notArchive := filepath.Join(t.TempDir(), "plain.pmtiles")
	if err := os.WriteFile(notArchive, []byte("SQLite format 3\x00padding padding"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsArchive(notArchive) {
		t.Fatal("non-PMTiles file with a .pmtiles name was detected as an archive")
	}
	if IsArchive(filepath.Join(t.TempDir(), "missing.pmtiles")) {
		t.Fatal("missing file reported as an archive")
	}
}

func TestArchiveRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	valid := pmtilestest.Options{
		TileType: pmtilestest.TileTypeMVT,
		Tiles:    []pmtilestest.Tile{{TileID: 0, Data: []byte("tile")}},
	}
	for _, test := range []struct {
		name    string
		mutate  func(raw []byte) []byte
		options *pmtilestest.Options
		wantErr string
	}{
		{
			name:    "wrong magic",
			options: &pmtilestest.Options{Magic: "NOTPMT!", TileType: pmtilestest.TileTypeMVT, Tiles: valid.Tiles},
			wantErr: "magic",
		},
		{
			name:    "unsupported version",
			options: &pmtilestest.Options{Version: 2, TileType: pmtilestest.TileTypeMVT, Tiles: valid.Tiles},
			wantErr: "version",
		},
		{
			name:    "brotli internal compression",
			options: &pmtilestest.Options{InternalCompression: pmtilestest.CompressionBrotli, TileType: pmtilestest.TileTypeMVT, Tiles: valid.Tiles},
			wantErr: "brotli",
		},
		{
			name:    "truncated below header",
			mutate:  func(raw []byte) []byte { return raw[:64] },
			wantErr: "shorter than",
		},
		{
			name:    "root directory past end of file",
			mutate:  func(raw []byte) []byte { return raw[:len(raw)-1] },
			wantErr: "past",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := valid
			if test.options != nil {
				options = *test.options
			}
			raw := pmtilestest.Bytes(t, options)
			if test.mutate != nil {
				raw = test.mutate(raw)
			}
			path := filepath.Join(t.TempDir(), "broken.pmtiles")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			archive, err := Open(path)
			if err == nil {
				err = archive.EachTile(context.Background(), func(Tile) error { return nil })
				archive.Close()
			}
			if err == nil {
				t.Fatalf("malformed archive accepted")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

// TestArchiveRejectsHeaderTileCountMismatch protects the traversal bound: a
// header that under-declares its tiles must not let directories address more.
func TestArchiveRejectsHeaderTileCountMismatch(t *testing.T) {
	t.Parallel()
	path := buildArchive(t, pmtilestest.Options{
		TileType:       pmtilestest.TileTypeMVT,
		MaxZoom:        1,
		AddressedTiles: 1,
		Tiles: []pmtilestest.Tile{
			{TileID: ZxyToID(1, 0, 0), Data: []byte("a")},
			{TileID: ZxyToID(1, 0, 1), Data: []byte("b")},
		},
	})
	archive, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer archive.Close()
	err = archive.EachTile(context.Background(), func(Tile) error { return nil })
	if err == nil {
		t.Fatal("directories addressing more tiles than the header accepted")
	}
	if !strings.Contains(err.Error(), "more tiles than") {
		t.Fatalf("error = %v", err)
	}
}

func TestEachTilePropagatesContextCancellation(t *testing.T) {
	t.Parallel()
	path := buildArchive(t, pmtilestest.Options{
		TileType: pmtilestest.TileTypeMVT,
		MaxZoom:  1,
		Tiles: []pmtilestest.Tile{
			{TileID: ZxyToID(1, 0, 0), Data: []byte("a")},
			{TileID: ZxyToID(1, 0, 1), Data: []byte("b")},
		},
	})
	archive, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer archive.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := archive.EachTile(ctx, func(Tile) error { return nil }); err == nil {
		t.Fatal("cancelled traversal returned no error")
	}
}
