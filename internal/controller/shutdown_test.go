package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// #58: a fleet/apply/power worker that outlives its caller is counted by
// the shutdown tracker, so Wait does not return while it still runs, and
// none starts once the Controller is stopping.
func TestWaitCountsDetachedWorkers(t *testing.T) {
	h := newHarness(t)
	reqCtx, hangUp := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(reqCtx)
	running, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_, _, ok := detached(h.c, r, func(context.Context) (struct{}, error) {
			close(running)
			<-release
			return struct{}{}, nil
		})
		if ok {
			t.Error("detached reported a result after the caller hung up")
		}
	}()
	<-running
	hangUp() // the caller disconnects; the worker runs on

	stopped := make(chan struct{})
	go func() { h.stop(); close(stopped) }()
	go func() { <-release; close(finished) }()
	select {
	case <-stopped:
		t.Fatal("Wait returned while a detached worker was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-stopped
	<-finished

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
