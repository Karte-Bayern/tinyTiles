package server

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tiles "github.com/SimonWaldherr/tinySQL/tiles"
)

func TestTileLoadGroupCoalescesConcurrentLoads(t *testing.T) {
	var group tileLoadGroup
	key := tiles.Key{Z: 8, X: 137, Y: 167}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) ([]byte, string, bool, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte{1, 2, 3}, "checksum", true, nil
	}

	const callers = 16
	type result struct {
		data     []byte
		checksum string
		found    bool
		err      error
	}
	results := make(chan result, callers)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		data, checksum, found, err := group.do(context.Background(), key, load)
		results <- result{data, checksum, found, err}
	}()
	<-started
	for index := 1; index < callers; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			data, checksum, found, err := group.do(context.Background(), key, load)
			results <- result{data, checksum, found, err}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		group.mu.Lock()
		waiters := group.loads[key].waiters
		group.mu.Unlock()
		if waiters == callers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", waiters, callers)
		}
		runtime.Gosched()
	}
	close(release)
	workers.Wait()
	close(results)
	if calls.Load() != 1 {
		t.Fatalf("load calls = %d, want 1", calls.Load())
	}
	for result := range results {
		if result.err != nil || !result.found || result.checksum != "checksum" || string(result.data) != string([]byte{1, 2, 3}) {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestTileLoadGroupWaiterHonorsCancellation(t *testing.T) {
	var group tileLoadGroup
	key := tiles.Key{Z: 8, X: 137, Y: 167}
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _, _, _ = group.do(context.Background(), key, func(context.Context) ([]byte, string, bool, error) {
			close(started)
			<-release
			return []byte{1}, "checksum", true, nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	followerDone := make(chan error, 1)
	go func() {
		_, _, _, err := group.do(ctx, key, nil)
		followerDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		group.mu.Lock()
		waiters := group.loads[key].waiters
		group.mu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want 2", waiters)
		}
		runtime.Gosched()
	}
	cancel()
	err := <-followerDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want context.Canceled", err)
	}
	close(release)
	<-leaderDone
}
