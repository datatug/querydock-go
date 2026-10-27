// Package composition owns production SDK resources and adapter wiring.
package composition

import (
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2firestore"
	"github.com/datatug/querydock-go/internal/identity"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/revisions"
)

var newFirestore = firestore.NewClientWithDatabase
var closeFirestore = (*firestore.Client).Close
var newIdentity = identity.NewProduction
var newStore = revisions.New

const DatabaseID = "(default)"

type ownedHandler struct {
	http.Handler
	close func() error
	once  sync.Once
	err   error
}

func (h *ownedHandler) Close() error {
	h.once.Do(func() { h.err = h.close() })
	return h.err
}

// Production requires explicit deployment configuration and uses ADC only.
// Construction performs no reads or writes; readiness describes local wiring.
func Production() (http.Handler, error) {
	return production(os.Getenv)
}
func production(getenv func(string) string) (http.Handler, error) {
	if getenv("QUERYDOCK_PROJECT_ID") != identity.ProjectID || getenv("QUERYDOCK_DATABASE_ID") != DatabaseID || getenv("FIREBASE_AUTH_EMULATOR_HOST") != "" || getenv("FIRESTORE_EMULATOR_HOST") != "" {
		return nil, publishing.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := newFirestore(ctx, identity.ProjectID, DatabaseID)
	if err != nil {
		return nil, publishing.ErrUnavailable
	}
	return wire(ctx, client, dalgo2firestore.NewDatabase(identity.ProjectID, client))
}
func wire(ctx context.Context, client *firestore.Client, db dal.DB) (http.Handler, error) {
	closeClient := func() error { return closeFirestore(client) }
	store, err := newStore(db)
	if err != nil {
		_ = closeClient()
		return nil, publishing.ErrUnavailable
	}
	verifier, err := newIdentity(ctx, identity.ProjectID)
	if err != nil {
		_ = closeClient()
		return nil, publishing.ErrUnavailable
	}
	api := publishing.Handler(publishing.Dependencies{Identity: verifier, Store: store, Clock: publishing.SystemClock{}, IDs: publishing.SecureIDs{Reader: rand.Reader}})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", api)
	return &ownedHandler{Handler: mux, close: closeClient}, nil
}
