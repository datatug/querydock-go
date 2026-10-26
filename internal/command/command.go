// Package command keeps process IO and listener lifetime outside business logic.
package command

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Run accepts narrow process seams so command/config/error paths are tested
// without opening a listener or exiting the test process.
func Run(getenv func(string) string, compose func() (http.Handler, error), serve func(*http.Server) error, report func(error)) int {
	port := getenv("PORT")
	if port == "" {
		port = "8080"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		report(errors.New("PORT must be an integer in 1..65535"))
		return 1
	}
	handler, err := compose()
	if err != nil {
		report(fmt.Errorf("querydock configuration: %w", err))
		return 1
	}
	server := &http.Server{Addr: ":" + strconv.Itoa(n), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	if err := serve(server); err != nil && !errors.Is(err, http.ErrServerClosed) {
		report(fmt.Errorf("querydock listener: %w", err))
		return 1
	}
	return 0
}
