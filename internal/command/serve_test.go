package command

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestServeLifecycle(t *testing.T) {
	oldNotify, oldListen, oldShutdown, oldClose := notifyContext, listen, shutdown, forceClose
	t.Cleanup(func() { notifyContext, listen, shutdown, forceClose = oldNotify, oldListen, oldShutdown, oldClose })
	server := &http.Server{}
	want := errors.New("listener detail")
	listen = func(*http.Server) error { return want }
	if err := serveContext(context.Background(), server, time.Second); err != want {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan struct{})
		closed := false
		stopped := false
		listen = func(*http.Server) error { <-done; return http.ErrServerClosed }
		shutdown = func(s *http.Server, c context.Context) error {
			if s != server {
				t.Fatal("wrong server")
			}
			if _, ok := c.Deadline(); !ok {
				t.Fatal("unbounded drain")
			}
			close(done)
			if fail {
				return context.DeadlineExceeded
			}
			return nil
		}
		forceClose = func(*http.Server) error { closed = true; return errors.New("close detail") }
		notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
			return ctx, func() { stopped = true }
		}
		if err := Serve(server); err != http.ErrServerClosed || closed != fail || !stopped {
			t.Fatal("signal/drain lost", err, closed, stopped)
		}
	}
}

type closingHandler struct {
	err    error
	closed bool
}

func (h *closingHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (h *closingHandler) Close() error                                 { h.closed = true; return h.err }
func TestRunCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := &closingHandler{}
		if fail {
			h.err = errors.New("secret")
		}
		reports := 0
		code := Run(func(string) string { return "8080" }, func() (http.Handler, error) { return h, nil }, func(*http.Server) error { return nil }, func(e error) {
			reports++
			if e.Error() != "querydock cleanup failed" {
				t.Fatal(e)
			}
		})
		if code != map[bool]int{false: 0, true: 1}[fail] || !h.closed || reports != map[bool]int{false: 0, true: 1}[fail] {
			t.Fatal("cleanup lost")
		}
	}
}

func TestDrainDeadlineExpires(t *testing.T) {
	oldListen, oldShutdown, oldClose := listen, shutdown, forceClose
	t.Cleanup(func() { listen, shutdown, forceClose = oldListen, oldShutdown, oldClose })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	forced := false
	listen = func(*http.Server) error { <-done; return http.ErrServerClosed }
	shutdown = func(_ *http.Server, drain context.Context) error { <-drain.Done(); return drain.Err() }
	forceClose = func(*http.Server) error { forced = true; close(done); return nil }
	if err := serveContext(ctx, &http.Server{}, time.Millisecond); err != http.ErrServerClosed || !forced {
		t.Fatal("deadline did not force close", err)
	}
}
