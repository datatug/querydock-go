// Package composition is the only production wiring boundary. Authentication
// and immutable revision libraries remain unwired until all safety gates pass.
package composition

import (
	"net/http"

	"github.com/datatug/querydock-go/internal/publishing"
)

// Production refuses startup until transactional quotas/tombstones (Task 3) and
// the matching private Firestore rule deployment (Task 6) are verified.
// There is no development-token, emulator or unbounded memory-store fallback.
func Production() (http.Handler, error) { return nil, publishing.ErrUnavailable }
