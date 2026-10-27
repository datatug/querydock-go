package command

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var notifyContext = signal.NotifyContext
var listen = (*http.Server).ListenAndServe
var shutdown = (*http.Server).Shutdown
var forceClose = (*http.Server).Close

// Serve drains in-flight requests for at most ten seconds on termination.
func Serve(server *http.Server) error {
	ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveContext(ctx, server, 10*time.Second)
}
func serveContext(ctx context.Context, server *http.Server, timeout time.Duration) error {
	result := make(chan error, 1)
	go func() { result <- listen(server) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if shutdown(server, drain) != nil {
			_ = forceClose(server)
		}
		return <-result
	}
}
