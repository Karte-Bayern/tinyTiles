//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestCommandImportRejectsInvalidResourceLimits(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "negative batch", args: []string{"--batch=-1"}, want: "batch must be zero (automatic) or positive"},
		{name: "zero memory", args: []string{"--max-memory=0"}, want: "max-memory must be positive"},
		{name: "negative memory", args: []string{"--max-memory=-1"}, want: "max-memory must be positive"},
		{name: "negative disk reserve", args: []string{"--min-free=-1"}, want: "min-free must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"import"}, tc.args...)
			args = append(args, "missing.mbtiles", "target.ttiles")
			if code := run(args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestCommandImportRejectsSourceArtifactOverlap(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	reject := func(t *testing.T, input, artifact string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run([]string{"import", "--replace", "--min-free=0", input, artifact}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "must not refer to import source") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("source was changed: info=%v err=%v", info, err)
		}
	}

	reject(t, source, source)
	t.Run("symlink aliases", func(t *testing.T) {
		sourceAlias := filepath.Join(temp, "source-alias.mbtiles")
		artifactAlias := filepath.Join(temp, "artifact-alias.ttiles")
		if err := os.Symlink(source, sourceAlias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink(source, artifactAlias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		reject(t, source, artifactAlias)
		reject(t, sourceAlias, source)
	})
	t.Run("hard link alias", func(t *testing.T) {
		artifactAlias := filepath.Join(temp, "artifact-hard-link.ttiles")
		if err := os.Link(source, artifactAlias); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		reject(t, source, artifactAlias)
	})
	t.Run("artifact contains source", func(t *testing.T) {
		containedSourceDir := filepath.Join(temp, "contained-source")
		if err := os.Mkdir(containedSourceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		containedSource := filepath.Join(containedSourceDir, "source.mbtiles")
		if err := createFlatMBTilesFixture(containedSource); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{"import", "--replace", "--min-free=0", containedSource, containedSourceDir}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "must not contain import source") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if info, err := os.Stat(containedSource); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("source was changed: info=%v err=%v", info, err)
		}
	})
}

func TestCommandImportRejectsNonRegularSource(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"import", "--min-free=0", t.TempDir(), filepath.Join(t.TempDir(), "dataset.ttiles")}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "expected a regular file") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestImportArtifactHonorsCancelledContext(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "dataset.ttiles")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := importArtifact(ctx, source, artifact, tiles.SchemaAuto, 1, 1<<20, 0, false, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("import error=%v, want context cancellation", err)
	}
	if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact exists after cancelled import: %v", err)
	}
}
