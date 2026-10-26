package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type closer struct {
	io.Reader
	err    error
	closed bool
}

func (c *closer) Close() error { c.closed = true; return c.err }
func TestRun(t *testing.T) {
	for _, kind := range []string{"success", "args", "open", "profile", "close"} {
		t.Run(kind, func(t *testing.T) {
			paths := []string{"profile"}
			file := &closer{Reader: strings.NewReader("mode: count\na.go:1.1,2.2 1 1\n")}
			var openErr error
			if kind == "args" {
				paths = nil
			}
			if kind == "open" {
				openErr = errors.New("open failed")
			}
			if kind == "profile" {
				file.Reader = strings.NewReader("mode: count\na.go:1.1,2.2 1 0\n")
			}
			if kind == "close" {
				file.err = errors.New("close failed")
			}
			var out bytes.Buffer
			code := run(paths, func(path string) (io.ReadCloser, error) {
				if path != "profile" {
					t.Fatal(path)
				}
				return file, openErr
			}, &out)
			if kind == "success" {
				if code != 0 || out.Len() != 0 || !file.closed {
					t.Fatal("gate success failed")
				}
			} else {
				if code != 1 || out.Len() == 0 {
					t.Fatal("failure ignored")
				}
				if kind != "args" && kind != "open" && !file.closed {
					t.Fatal("profile left open")
				}
			}
		})
	}
}
func TestMain(t *testing.T) {
	oldArgs, oldOutput, oldExit := os.Args, output, exit
	t.Cleanup(func() { os.Args, output, exit = oldArgs, oldOutput, oldExit })
	path := t.TempDir() + "/profile"
	if err := os.WriteFile(path, []byte("mode: count\na.go:1.1,2.2 1 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"coveragegate", path}
	var out bytes.Buffer
	output = &out
	code := -1
	exit = func(c int) { code = c }
	main()
	if code != 0 || out.Len() != 0 {
		t.Fatalf("main %d %s", code, &out)
	}
	if _, err := open(path + "missing"); err == nil {
		t.Fatal("missing file opened")
	}
}
