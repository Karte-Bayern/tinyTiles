//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	tinytiles "github.com/Karte-Bayern/tinyTiles"
	"github.com/Karte-Bayern/tinyTiles/internal/pmtiles"
	"github.com/Karte-Bayern/tinyTiles/internal/pmtiles/pmtilestest"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// TestImportPMTilesArchivePublishesArtifact runs a PMTiles archive through the
// real import command path and reads the published artifact back.
//
// The central assertion is the coordinate convention. PMTiles addresses tiles
// in XYZ (origin top left) and tinyTiles stores TMS (origin bottom left), so a
// missing or doubled row flip would still produce a plausible-looking artifact
// with every tile vertically mirrored. Distinct payloads per row catch that.
func TestImportPMTilesArchivePublishesArtifact(t *testing.T) {
	workDir := t.TempDir()
	source := filepath.Join(workDir, "fixture.pmtiles")
	pmtilestest.Build(t, source, pmtilestest.Options{
		InternalCompression: pmtilestest.CompressionGzip,
		TileType:            pmtilestest.TileTypeMVT,
		MinZoom:             1,
		MaxZoom:             1,
		Metadata:            `{"name":"pmtiles fixture","attribution":"© test","vector_layers":[{"id":"roads"}],"tilestats":{"layerCount":1}}`,
		Tiles: []pmtilestest.Tile{
			// XYZ row 0 is the northern row; XYZ row 1 is the southern one.
			{TileID: pmtiles.ZxyToID(1, 0, 0), Data: []byte("north-west")},
			{TileID: pmtiles.ZxyToID(1, 0, 1), Data: []byte("south-west")},
		},
	})

	artifact := filepath.Join(workDir, "fixture.ttiles")
	var stdout bytes.Buffer
	if code := commandImport([]string{"--min-free", "0", source, artifact}, &stdout, &stdout); code != 0 {
		t.Fatalf("import exited %d: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "phase=pmtiles") {
		t.Fatalf("import did not report the PMTiles staging phase: %s", stdout.String())
	}

	dataset, err := tinytiles.Open(context.Background(), artifact, tinytiles.OpenOptions{Readers: 1})
	if err != nil {
		t.Fatalf("open published artifact: %v", err)
	}
	defer dataset.Close()

	// XYZ (1,0,0) is TMS row 1; XYZ (1,0,1) is TMS row 0.
	for _, want := range []struct {
		key     tiles.Key
		payload string
	}{
		{key: tiles.Key{Z: 1, X: 0, Y: 1}, payload: "north-west"},
		{key: tiles.Key{Z: 1, X: 0, Y: 0}, payload: "south-west"},
	} {
		tile, found, err := dataset.LookupTMS(context.Background(), want.key)
		if err != nil || !found {
			t.Fatalf("TMS %v: found=%t err=%v", want.key, found, err)
		}
		if string(tile.Data) != want.payload {
			t.Fatalf("TMS %v carries %q, want %q (row flip is wrong)", want.key, tile.Data, want.payload)
		}
	}

	// The XYZ accessor must return the original PMTiles orientation.
	tile, found, err := dataset.LookupXYZ(context.Background(), 1, 0, 0)
	if err != nil || !found || string(tile.Data) != "north-west" {
		t.Fatalf("XYZ 1/0/0 = %q found=%t err=%v, want north-west", tile.Data, found, err)
	}

	metadata, err := dataset.Metadata()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if metadata["format"] != "pbf" {
		t.Fatalf("format metadata = %q, want pbf", metadata["format"])
	}
	if metadata["name"] != "pmtiles fixture" || metadata["attribution"] != "© test" {
		t.Fatalf("PMTiles JSON metadata was not relayed: %#v", metadata)
	}
	if metadata["kb:content_encoding"] != "" {
		t.Fatalf("uncompressed tiles must not declare a content encoding: %q", metadata["kb:content_encoding"])
	}
	// vector_layers and tilestats belong in the standard MBTiles json row, which
	// the server relays into TileJSON.
	var tileset struct {
		VectorLayers []struct {
			ID string `json:"id"`
		} `json:"vector_layers"`
		Tilestats map[string]any `json:"tilestats"`
	}
	if err := json.Unmarshal([]byte(metadata["json"]), &tileset); err != nil {
		t.Fatalf("decode json metadata row %q: %v", metadata["json"], err)
	}
	if len(tileset.VectorLayers) != 1 || tileset.VectorLayers[0].ID != "roads" || tileset.Tilestats == nil {
		t.Fatalf("vector layer metadata not relayed: %#v", tileset)
	}
}

// TestImportPMTilesPreservesGzipTilePayloads checks that a gzip-compressed
// archive keeps its payload bytes and declares the encoding, rather than
// being silently decompressed or relabelled.
func TestImportPMTilesPreservesGzipTilePayloads(t *testing.T) {
	workDir := t.TempDir()
	source := filepath.Join(workDir, "gz.pmtiles")
	payload := []byte("\x1f\x8b\x08 pretend gzip body")
	pmtilestest.Build(t, source, pmtilestest.Options{
		TileType:        pmtilestest.TileTypeMVT,
		TileCompression: pmtilestest.CompressionGzip,
		Tiles:           []pmtilestest.Tile{{TileID: pmtiles.ZxyToID(0, 0, 0), Data: payload}},
	})
	artifact := filepath.Join(workDir, "gz.ttiles")
	var stdout bytes.Buffer
	if code := commandImport([]string{"--min-free", "0", source, artifact}, &stdout, &stdout); code != 0 {
		t.Fatalf("import exited %d: %s", code, stdout.String())
	}
	dataset, err := tinytiles.Open(context.Background(), artifact, tinytiles.OpenOptions{Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer dataset.Close()
	tile, found, err := dataset.LookupTMS(context.Background(), tiles.Key{Z: 0, X: 0, Y: 0})
	if err != nil || !found {
		t.Fatalf("lookup: found=%t err=%v", found, err)
	}
	if !bytes.Equal(tile.Data, payload) {
		t.Fatalf("tile payload = %q, want the stored bytes unchanged", tile.Data)
	}
	metadata, err := dataset.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if metadata["kb:content_encoding"] != "gzip" {
		t.Fatalf("gzip tile encoding not declared: %#v", metadata)
	}
}

func TestImportPMTilesRejectsUndecodableCompression(t *testing.T) {
	workDir := t.TempDir()
	source := filepath.Join(workDir, "zstd.pmtiles")
	pmtilestest.Build(t, source, pmtilestest.Options{
		TileType:        pmtilestest.TileTypeMVT,
		TileCompression: pmtilestest.CompressionZstd,
		Tiles:           []pmtilestest.Tile{{TileID: 0, Data: []byte("body")}},
	})
	var stdout bytes.Buffer
	code := commandImport([]string{"--min-free", "0", source, filepath.Join(workDir, "out.ttiles")}, &stdout, &stdout)
	if code == 0 {
		t.Fatalf("zstd tile compression accepted: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "zstd") {
		t.Fatalf("error does not name the unsupported compression: %s", stdout.String())
	}
}
