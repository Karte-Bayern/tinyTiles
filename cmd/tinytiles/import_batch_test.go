//go:build sqliteimport && !js && !wasm && !baremetal

package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAutoImportBatchSizeUsesBoundedSample(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	largest := int64(10_000)
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tiles SET tile_data=?`, make([]byte, largest)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	const want = 2_048
	memory := importBatchFixedMemory + (largest*importBatchSampleHeadroom+importBatchPerRowOverhead)*want
	got, err := autoImportBatchSize(context.Background(), source, memory)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("automatic batch=%d, want %d", got, want)
	}

	const smallerBatch = 37
	got, err = autoImportBatchSize(context.Background(), source, importBatchFixedMemory+(largest*importBatchSampleHeadroom+importBatchPerRowOverhead)*smallerBatch)
	if err != nil {
		t.Fatal(err)
	}
	if got != smallerBatch {
		t.Fatalf("automatic smaller batch=%d, want %d", got, smallerBatch)
	}
}

func TestCommandImportAutoBatch(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "dataset.ttiles")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--batch=0", "--min-free=0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	batch, err := autoImportBatchSize(context.Background(), source, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if batch <= 1_000 || !strings.Contains(stdout.String(), "batch="+strconv.Itoa(batch)) || !strings.Contains(stdout.String(), "batches-of="+strconv.Itoa(batch)) {
		t.Fatalf("automatic batch was not reported: %q", stdout.String())
	}
}

func TestCommandImportKeepsExplicitBatch(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "dataset.ttiles")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--batch=2", "--min-free=0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "batch=2") || !strings.Contains(stdout.String(), "batches-of=2") {
		t.Fatalf("explicit batch was changed: %q", stdout.String())
	}
}

func TestCommandImportKeepsDefaultBatch(t *testing.T) {
	temp := t.TempDir()
	source := filepath.Join(temp, "source.mbtiles")
	if err := createFlatMBTilesFixture(source); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(temp, "dataset.ttiles")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"import", "--min-free=0", source, artifact}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "batch=1000") || !strings.Contains(stdout.String(), "batches-of=1000") {
		t.Fatalf("default batch changed: %q", stdout.String())
	}
}
