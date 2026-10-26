package main

import (
	"net/http"
	"testing"
)

func TestMainComposition(t *testing.T) {
	oldExit := exit
	t.Cleanup(func() { exit = oldExit })
	code := -1
	exit = func(c int) { code = c }
	main()
	if code != 1 {
		t.Fatal("unconfigured production must exit nonzero")
	}
}
func TestMainSuccess(t *testing.T) {
	oldGetenv, oldCompose, oldServe, oldReport, oldExit := getenv, compose, serve, report, exit
	t.Cleanup(func() { getenv, compose, serve, report, exit = oldGetenv, oldCompose, oldServe, oldReport, oldExit })
	getenv = func(string) string { return "8080" }
	compose = func() (http.Handler, error) { return http.NotFoundHandler(), nil }
	served := false
	serve = func(s *http.Server) error {
		served = true
		if s.Addr != ":8080" {
			t.Fatal(s.Addr)
		}
		return http.ErrServerClosed
	}
	report = func(err error) { t.Fatalf("unexpected %v", err) }
	code := -1
	exit = func(c int) { code = c }
	main()
	if !served || code != 0 {
		t.Fatal("main lost command result")
	}
}
