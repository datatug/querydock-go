package main

import (
	"fmt"
	"io"
	"os"

	"github.com/datatug/querydock-go/internal/coveragegate"
)

var (
	args             = func() []string { return os.Args[1:] }
	open             = func(path string) (io.ReadCloser, error) { return os.Open(path) }
	output io.Writer = os.Stderr
	exit             = os.Exit
)

func run(paths []string, read func(string) (io.ReadCloser, error), stderr io.Writer) int {
	if len(paths) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: coveragegate coverage.out")
		return 1
	}
	file, err := read(paths[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	err = coveragegate.Check(file)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func main() { exit(run(args(), open, output)) }
