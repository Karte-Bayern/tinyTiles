//go:build !js && !wasm && !baremetal

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func commandValidate(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: tinytiles validate dataset.ttiles/")
		return 2
	}
	start := time.Now()
	manifest, err := tiles.ValidateArtifact(context.Background(), args[0])
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles validate: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "valid schema=%s tables=%d elapsed=%s\n", manifest.Schema, len(manifest.Tables), time.Since(start).Round(time.Millisecond))
	return 0
}

func commandInspect(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: tinytiles inspect dataset.ttiles/")
		return 2
	}
	manifest, err := tiles.ValidateArtifact(context.Background(), args[0])
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles inspect: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		fmt.Fprintf(stderr, "tinytiles inspect: %v\n", err)
		return 1
	}
	return 0
}

func commandTile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	memory := fs.Int64("max-memory", 64<<20, "maximum reader cache budget in bytes")
	out := fs.String("out", "", "write tile bytes to this file instead of stdout")
	scheme := fs.String("scheme", "tms", "input coordinate scheme: tms or xyz")
	if fs.Parse(args) != nil || fs.NArg() != 4 {
		fmt.Fprintln(stderr, "usage: tinytiles tile [-out file] [-scheme tms|xyz] dataset.ttiles/ z x y")
		return 2
	}
	if *scheme != "tms" && *scheme != "xyz" {
		fmt.Fprintln(stderr, "tinytiles tile: scheme must be tms or xyz")
		return 2
	}
	if *memory < 0 {
		fmt.Fprintln(stderr, "tinytiles tile: max-memory must not be negative")
		return 2
	}
	var coords [3]int
	for i := range coords {
		n, err := strconv.Atoi(fs.Arg(i + 1))
		if err != nil {
			fmt.Fprintf(stderr, "tinytiles tile: z, x and y must be integers: %v\n", err)
			return 2
		}
		coords[i] = n
	}
	z, x, y := coords[0], coords[1], coords[2]
	key := tiles.Key{Z: z, X: x, Y: y}
	if err := key.Validate(); err != nil {
		fmt.Fprintf(stderr, "tinytiles tile: %v\n", err)
		return 2
	}
	if *scheme == "xyz" {
		key.Y = (1 << z) - 1 - y
	}
	reader, err := tiles.OpenArtifact(context.Background(), fs.Arg(0), tiles.OpenOptions{MaxMemoryBytes: *memory})
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles tile: %v\n", err)
		return 1
	}
	defer reader.Close()
	tile, found, err := reader.Lookup(context.Background(), key)
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles tile: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "tinytiles tile: not found: %d/%d/%d\n", z, x, y)
		return 3
	}
	if *out == "" {
		var n int
		n, err = stdout.Write(tile.Data)
		if err == nil && n != len(tile.Data) {
			err = io.ErrShortWrite
		}
	} else {
		err = os.WriteFile(*out, tile.Data, 0o644)
	}
	if err != nil {
		fmt.Fprintf(stderr, "tinytiles tile: write: %v\n", err)
		return 1
	}
	return 0
}
