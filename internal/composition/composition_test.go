package composition

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/firestore"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2firestore"
	"github.com/datatug/querydock-go/internal/identity"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/revisions"
	"google.golang.org/api/option"
)

func TestProductionFailsClosed(t *testing.T) {
	t.Setenv("QUERYDOCK_PROJECT_ID", "")
	handler, err := Production()
	if handler != nil || !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("unsafe production")
	}
}
func TestProductionWiring(t *testing.T) {
	oldFS, oldClose, oldID, oldStore := newFirestore, closeFirestore, newIdentity, newStore
	t.Cleanup(func() { newFirestore, closeFirestore, newIdentity, newStore = oldFS, oldClose, oldID, oldStore })
	config := map[string]string{"QUERYDOCK_PROJECT_ID": identity.ProjectID, "QUERYDOCK_DATABASE_ID": DatabaseID}
	getenv := func(k string) string { return config[k] }
	calls, closed := 0, 0
	fakeFirestore := func(ctx context.Context, p, d string, opts ...option.ClientOption) (*firestore.Client, error) {
		calls++
		if p != identity.ProjectID || d != DatabaseID || len(opts) != 0 {
			t.Fatal("wrong ADC boundary")
		}
		return &firestore.Client{}, nil
	}
	newFirestore = fakeFirestore
	closeFirestore = func(*firestore.Client) error { closed++; return errors.New("private detail") }
	newStore = func(dal.DB) (*revisions.Store, error) { return nil, nil }
	newIdentity = func(context.Context, string) (*identity.Verifier, error) { return nil, nil }
	for key, bad := range map[string]string{"QUERYDOCK_PROJECT_ID": "wrong", "QUERYDOCK_DATABASE_ID": "wrong", "FIRESTORE_EMULATOR_HOST": "host", "FIREBASE_AUTH_EMULATOR_HOST": "host"} {
		prior := config[key]
		config[key] = bad
		if h, e := production(getenv); h != nil || e != publishing.ErrUnavailable {
			t.Fatal("invalid accepted")
		}
		config[key] = prior
	}
	if calls != 0 {
		t.Fatal("SDK before validation")
	}
	newFirestore = func(context.Context, string, string, ...option.ClientOption) (*firestore.Client, error) {
		return nil, errors.New("private detail")
	}
	if h, e := production(getenv); h != nil || e != publishing.ErrUnavailable {
		t.Fatal("constructor error exposed")
	}
	newFirestore = fakeFirestore
	newStore = func(dal.DB) (*revisions.Store, error) { return nil, errors.New("private detail") }
	if h, e := production(getenv); h != nil || e != publishing.ErrUnavailable || closed != 1 {
		t.Fatal("store cleanup")
	}
	newStore = func(dal.DB) (*revisions.Store, error) { return nil, nil }
	newIdentity = func(context.Context, string) (*identity.Verifier, error) { return nil, errors.New("private detail") }
	if h, e := production(getenv); h != nil || e != publishing.ErrUnavailable || closed != 2 {
		t.Fatal("identity cleanup")
	}
	newIdentity = func(ctx context.Context, project string) (*identity.Verifier, error) {
		if project != identity.ProjectID {
			t.Fatal("wrong identity project")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded startup")
		}
		return &identity.Verifier{}, nil
	}
	newStore = revisions.New
	h, e := production(getenv)
	if e != nil {
		t.Fatal(e)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Fatal("readiness leaked")
	}
	owned := h.(*ownedHandler)
	if owned.Close() == nil || owned.Close() == nil || closed != 3 {
		t.Fatal("close ownership")
	}
}

// SDK construction and cleanup do not require or perform any Firestore writes.
func TestSDKConstructorLifetime(t *testing.T) {
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := newFirestore(ctx, identity.ProjectID, DatabaseID, option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if err := closeFirestore(client); err != nil {
		t.Fatal(err)
	}
}
func TestConcreteStoreWiring(t *testing.T) {
	oldID := newIdentity
	t.Cleanup(func() { newIdentity = oldID })
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	client, err := newFirestore(context.Background(), identity.ProjectID, DatabaseID, option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	newIdentity = func(context.Context, string) (*identity.Verifier, error) { return &identity.Verifier{}, nil }
	h, err := wire(context.Background(), client, dalgo2firestore.NewDatabase(identity.ProjectID, client))
	if err != nil {
		t.Fatal(err)
	}
	defer h.(*ownedHandler).Close()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/querydock/fiddles", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatal(rr.Code)
	}
}
