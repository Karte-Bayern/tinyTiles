//go:build js || wasm || baremetal

package tinytiles

import (
	"errors"
	"testing"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestBuildPBFReportsUnsupportedArtifactTarget(t *testing.T) {
	_, err := BuildPBF(t.Context(), PBFBuildOptions{})
	if !errors.Is(err, tiles.ErrArtifactImportUnavailable) {
		t.Fatalf("BuildPBF error = %v, want ErrArtifactImportUnavailable", err)
	}
}
