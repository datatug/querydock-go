// Package composition is the only production wiring boundary. Authentication
// and immutable revision libraries remain unwired until all safety gates pass.
package composition

import (
	"net/http"

	"github.com/datatug/querydock-go/internal/publishing"
)

// Production remains disabled until Task 6 verifies live private Firestore
// rules and explicitly wires the reviewed identity and bounded storage adapters.
// There is no development-token, emulator or unbounded memory-store fallback.
func Production() (http.Handler, error) { return nil, publishing.ErrUnavailable }
