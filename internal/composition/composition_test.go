package composition

import (
	"errors"
	"github.com/datatug/querydock-go/internal/publishing"
	"testing"
)

func TestProductionFailsClosed(t *testing.T) {
	handler, err := Production()
	if handler != nil || !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("unconfigured production unexpectedly usable")
	}
}
