package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A fleet/apply/power worker that outlives its caller is counted by
// the shutdown tracker, so Wait does not return while it still runs, and
// none starts once the Controller is stopping, even when Wait begins before
// the context is cancelled.
func TestWaitCountsDetachedWorkers(t *testing.T) {
	h := newHarness(t)
	reqCtx, hangUp := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(reqCtx)
	running, release, callerGone := make(chan struct{}), make(chan struct{}), make(chan bool)
	go func() {
		_, _, ok := detached(h.c, r, func(context.Context) (struct{}, error) {
			close(running)
			<-release
			return struct{}{}, nil
		})
		callerGone <- ok
	}()
	<-running
	hangUp() // the caller disconnects; the worker runs on
	if <-callerGone {
		t.Fatal("detached reported a result after the caller hung up")
	}

	stopped := make(chan struct{})
	go func() { h.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Wait returned while a detached worker was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-stopped

	ran := false
	_, err, ok := detached(h.c, httptest.NewRequest(http.MethodPost, "/", nil), func(context.Context) (struct{}, error) {
		ran = true
		return struct{}{}, nil
	})
	var he *httpError
	if !ok || ran || !errors.As(err, &he) || he.status != http.StatusServiceUnavailable {
		t.Fatalf("after stop: ok=%v ran=%v err=%v; want a 503 and nothing run", ok, ran, err)
	}
}

// Admission is closed by Wait itself, so a worker cannot be added
// between the context check and Add once Wait has begun.
func TestSpawnRefusedOnceWaitBegins(t *testing.T) {
	c := &Controller{}
	c.Wait()
	if c.spawn(func() { t.Error("spawned after Wait") }) {
		t.Fatal("spawn admitted a worker after Wait began")
	}
}
