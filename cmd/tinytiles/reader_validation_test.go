//go:build !js && !wasm && !baremetal

package main

import (
	"bytes"
	"testing"
)

func TestTileRejectsInvalidOptionsBeforeOpeningArtifact(t *testing.T) {
	for _, args := range [][]string{
		{"-scheme", "invalid", "missing.ttiles", "0", "0", "0"},
		{"-max-memory", "-1", "missing.ttiles", "0", "0", "0"},
		{"missing.ttiles", "-1", "0", "0"},
		{"missing.ttiles", "1", "2", "0"},
		{"-scheme", "xyz", "missing.ttiles", "1", "0", "2"},
	} {
		var stdout, stderr bytes.Buffer
		if code := commandTile(args, &stdout, &stderr); code != 2 {
			t.Errorf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
	}
}
