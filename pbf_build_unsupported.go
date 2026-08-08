//go:build js || wasm || baremetal

package tinytiles

import (
	"context"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// BuildPBF is unavailable on browser and bare-metal targets. Serving an
// already published artifact remains available where the reader is supported.
func BuildPBF(ctx context.Context, _ PBFBuildOptions) (PBFBuildResult, error) {
	if err := ctx.Err(); err != nil {
		return PBFBuildResult{}, err
	}
	return PBFBuildResult{}, tiles.ErrArtifactImportUnavailable
}
