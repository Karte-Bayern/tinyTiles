package server

import (
	"context"
	"sync"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

// tileLoadGroup coalesces concurrent cache misses for one immutable tile.
// A newly opened map or several clients can request the same cold tile at
// once; running the pager lookup and SHA-256 once avoids multiplying the most
// expensive part of that burst. The completed payload is caller-owned and is
// safe to share because Server never mutates it.
type tileLoadGroup struct {
	mu    sync.Mutex
	loads map[tiles.Key]*tileLoad
}

type tileLoad struct {
	done     chan struct{}
	waiters  int
	data     []byte
	checksum string
	found    bool
	err      error
}

func (g *tileLoadGroup) do(ctx context.Context, key tiles.Key, load func(context.Context) ([]byte, string, bool, error)) ([]byte, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", false, err
	}
	g.mu.Lock()
	if g.loads == nil {
		g.loads = make(map[tiles.Key]*tileLoad)
	}
	if existing := g.loads[key]; existing != nil {
		existing.waiters++
		g.mu.Unlock()
		select {
		case <-existing.done:
			return existing.data, existing.checksum, existing.found, existing.err
		case <-ctx.Done():
			return nil, "", false, ctx.Err()
		}
	}
	call := &tileLoad{done: make(chan struct{}), waiters: 1}
	g.loads[key] = call
	g.mu.Unlock()

	// The first caller may disconnect while the local artifact lookup is in
	// progress. Completing that bounded read still warms the immutable cache
	// and lets the already-waiting callers use the same result. Dataset.Close
	// continues to terminate the reader pool independently of this context.
	data, checksum, found, err := load(context.WithoutCancel(ctx))

	g.mu.Lock()
	call.data, call.checksum, call.found, call.err = data, checksum, found, err
	delete(g.loads, key)
	close(call.done)
	g.mu.Unlock()
	return data, checksum, found, err
}
