//go:build sqliteimport && !windows

package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	_ "github.com/SimonWaldherr/tinySQL/importer"
	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// writeFixture (re)builds a one-tile MBTiles source with the given payload and
// imports it into artifact, replacing any artifact already published there.
// This mirrors an operator running `tinytiles import --replace` against a
// live deployment path.
func writeFixture(t *testing.T, source, artifact string, payload []byte) {
	t.Helper()
	_ = os.Remove(source)
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE metadata (name TEXT, value TEXT);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB);
		INSERT INTO metadata VALUES ('name', 'reload-fixture'), ('format', 'pbf'), ('minzoom', '2'), ('maxzoom', '2');
		INSERT INTO tiles VALUES (2, 1, 2, ?);`, payload)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tiles.ImportMBTiles(context.Background(), source, artifact, &tiles.ImportOptions{
		Schema: tiles.SchemaFlat, BatchSize: 1, MinFreeBytes: 0, ReplaceExisting: true,
	}); err != nil {
		t.Fatalf("import into %s: %v", artifact, err)
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestSIGHUPReloadsPublishedRevision builds the real tinytiles-server binary
// (without the sqliteimport tag, matching its shipped shape), starts it
// against a published artifact, republishes a new revision to the same path
// with ReplaceExisting, and sends SIGHUP. It asserts the running process picks
// up the new tile bytes without ever failing a request and exits cleanly on a
// subsequent SIGTERM.
func TestSIGHUPReloadsPublishedRevision(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a real subprocess; skipped with -short")
	}
	workDir := t.TempDir()
	source := filepath.Join(workDir, "fixture.mbtiles")
	artifact := filepath.Join(workDir, "fixture.ttiles")
	writeFixture(t, source, artifact, []byte{1, 2, 3})

	binary := filepath.Join(workDir, "tinytiles-server")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = os.Environ()
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tinytiles-server: %v\n%s", err, output)
	}

	addr := freeTCPAddr(t)
	base := "http://" + addr
	cmd := exec.Command(binary, "-artifact", artifact, "-dataset", "reload-test", "-addr", addr)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tinytiles-server: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, body := tryFetch(base); code == http.StatusOK && bytes.Equal(body, []byte{1, 2, 3}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never served the initial revision; stderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	writeFixture(t, source, artifact, []byte{9, 8, 7})
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}

	deadline = time.Now().Add(10 * time.Second)
	for {
		code, body := tryFetch(base)
		if code == http.StatusOK && bytes.Equal(body, []byte{9, 8, 7}) {
			break
		}
		if code != 0 && code != http.StatusOK {
			t.Fatalf("request failed with status %d during reload", code)
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never switched to the reloaded revision; stderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tinytiles-server exited with error: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("tinytiles-server did not exit after SIGTERM; stderr:\n%s", stderr.String())
	}
}

func tryFetch(base string) (int, []byte) {
	response, err := http.Get(base + "/tiles/2/1/1.mvt")
	if err != nil {
		return 0, nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil
	}
	return response.StatusCode, body
}
