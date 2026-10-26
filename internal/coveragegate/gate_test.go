package coveragegate

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type broken struct{}

func (broken) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestGate(t *testing.T) {
	for _, profile := range []string{"mode: set\na.go:1.1,2.2 3 1\n", "mode: count\na.go:1.1,2.2 3 0\na.go:1.1,2.2 3 4\n", "mode: atomic\na.go:1.1,2.2 3 2\n"} {
		if err := Check(strings.NewReader(profile)); err != nil {
			t.Fatalf("rejected full profile %v", err)
		}
	}
	for _, profile := range []string{"", "mode: wrong\n", "mode: count\n", "mode: count\nbad\n", "mode: count\na -1 1\n", "mode: count\na nope 1\n", "mode: count\na 1 nope\n", "mode: count\na 1 1\na 2 1\n"} {
		if err := Check(strings.NewReader(profile)); err == nil {
			t.Fatalf("accepted malformed profile %q", profile)
		}
	}
	if err := Check(io.MultiReader(strings.NewReader("mode: count\n"), broken{})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost reader failure %v", err)
	}
}

// This is a negative acceptance test: rounding would call 99999/100000 100.0%,
// but one actually unexecuted branch MUST fail, including when others are hit.
func TestOneUncoveredBranchFails(t *testing.T) {
	profile := "mode: count\ncovered.go:1.1,2.2 99999 8\nuncovered.go:3.1,4.2 1 0\n"
	err := Check(strings.NewReader(profile))
	if err == nil || !strings.Contains(err.Error(), "uncovered.go") {
		t.Fatalf("uncovered branch passed: %v", err)
	}
}
