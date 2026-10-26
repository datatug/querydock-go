// Package composition is the only production wiring boundary. Firebase/DALgo
// adapters are intentionally not implemented by the foundation task.
package composition

import (
	"net/http"

	"github.com/datatug/querydock-go/internal/publishing"
)

// Production refuses startup until verified identity and atomic durable storage
// are wired by Tasks 2–3. There is no development-token or memory-store fallback.
func Production() (http.Handler, error) { return nil, publishing.ErrUnavailable }
