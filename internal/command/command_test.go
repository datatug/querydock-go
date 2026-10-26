package command

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		name, port           string
		composeErr, serveErr error
		code                 int
	}{
		{name: "default"}, {name: "explicit", port: "9090"}, {name: "bad", port: "no", code: 1}, {name: "zero", port: "0", code: 1}, {name: "negative", port: "-1", code: 1}, {name: "large", port: "65536", code: 1}, {name: "composition", composeErr: errors.New("not configured"), code: 1}, {name: "listener", serveErr: errors.New("listen failed"), code: 1}, {name: "closed", serveErr: http.ErrServerClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reports []error
			composed, served := false, false
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			code := Run(func(key string) string {
				if key != "PORT" {
					t.Fatal("unexpected configuration lookup")
				}
				return tc.port
			}, func() (http.Handler, error) { composed = true; return handler, tc.composeErr }, func(server *http.Server) error {
				served = true
				port := tc.port
				if port == "" {
					port = "8080"
				}
				if server.Addr != ":"+port || server.Handler == nil || server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes != 16*1024 {
					t.Fatalf("unsafe server %+v", server)
				}
				return tc.serveErr
			}, func(err error) { reports = append(reports, err) })
			if code != tc.code {
				t.Fatalf("code %d want %d", code, tc.code)
			}
			if code == 1 && len(reports) != 1 {
				t.Fatal("error not reported")
			}
			if code == 0 && len(reports) != 0 {
				t.Fatal("success reported failure")
			}
			if tc.composeErr != nil && (!composed || served || !strings.Contains(reports[0].Error(), "not configured")) {
				t.Fatal("composition failure did not stop listener")
			}
			if !composed && served {
				t.Fatal("listener before configuration")
			}
		})
	}
}
