package publishing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/datatug/querydock-go/internal/snapshot"
)

const fiddleID = "abcdefghijklmnopqrstuv"

type verifier func(context.Context, string) (Identity, error)

func (f verifier) Verify(c context.Context, s string) (Identity, error) { return f(c, s) }

type clock struct{ at time.Time }

func (c clock) Now() time.Time { return c.at }

type ids func() (string, error)

func (f ids) NewID() (string, error) { return f() }

type store struct {
	writes     []Write
	deletes    []Delete
	reads      int
	err        error
	receipt    Receipt
	saved      PublicRevision
	panicValue any
}

func (s *store) Create(_ context.Context, w Write) (Receipt, error) {
	s.writes = append(s.writes, w)
	if s.panicValue != nil {
		panic(s.panicValue)
	}
	return s.receipt, s.err
}
func (s *store) Append(_ context.Context, w Write) (Receipt, error) {
	s.writes = append(s.writes, w)
	return s.receipt, s.err
}
func (s *store) Read(_ context.Context, _ string, _ uint64) (PublicRevision, error) {
	s.reads++
	return s.saved, s.err
}
func (s *store) Delete(_ context.Context, d Delete) error {
	s.deletes = append(s.deletes, d)
	return s.err
}
func fixture(t *testing.T) snapshot.Validated {
	t.Helper()
	raw, err := os.ReadFile("../snapshot/testdata/snapshot-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v []struct{ Source string }
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	s, err := snapshot.Import([]byte(v[0].Source))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func setup(t *testing.T) (Dependencies, *store, map[string]any) {
	t.Helper()
	s := fixture(t)
	db := &store{receipt: Receipt{ID: fiddleID, Revision: 1, Digest: s.Digest}, saved: PublicRevision{ID: fiddleID, Revision: 1, Snapshot: s}}
	deps := Dependencies{Identity: verifier(func(_ context.Context, token string) (Identity, error) {
		if token != "verified" {
			return Identity{}, ErrUnauthorized
		}
		return Identity{UID: "verified-owner"}, nil
	}), Clock: clock{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}, IDs: ids(func() (string, error) { return fiddleID, nil }), Store: db}
	return deps, db, map[string]any{"snapshot": json.RawMessage(s.JSON), "digest": s.Digest, "idempotencyKey": "retry-key"}
}
func body(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func request(h http.Handler, method, path, source, token, media string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(source))
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	if media != "" {
		r.Header.Set("Content-Type", media)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func expect(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d want %d: %s", w.Code, status, w.Body)
	}
	if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("missing code %s: %s", code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store, must-revalidate" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing no-cache/security headers")
	}
}
func TestWriteBoundary(t *testing.T) {
	deps, db, input := setup(t)
	h := Handler(deps)
	w := request(h, "POST", "/api/querydock/fiddles", body(t, input), "Bearer verified", "application/json; charset=utf-8")
	expect(t, w, 200, "")
	if len(db.writes) != 1 || db.writes[0].OwnerUID != "verified-owner" || db.writes[0].Snapshot != fixture(t) || db.writes[0].ExpectedRevision != 0 || db.writes[0].At != deps.Clock.Now() || db.writes[0].IdempotencyKey != "retry-key" {
		t.Fatalf("incorrect write %+v", db.writes)
	}
	var receipt Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.CanonicalURL != "https://querydock.run/sql/"+fiddleID+"/1" || receipt.Digest != fixture(t).Digest {
		t.Fatalf("bad receipt %+v", receipt)
	}
	input["expectedRevision"] = 1
	db.receipt.Revision = 2
	w = request(h, "POST", "/api/querydock/fiddles/"+fiddleID+"/revisions", body(t, input), "bearer verified", "application/json")
	expect(t, w, 200, "")
	if len(db.writes) != 2 || db.writes[1].ExpectedRevision != 1 || db.writes[1].ID != fiddleID {
		t.Fatal("append lost expected revision")
	}
}
func TestRejectWritesBeforePersistence(t *testing.T) {
	tests := []struct {
		name, source, token, media, path, code string
		status                                 int
		alter                                  func(*Dependencies, map[string]any)
	}{
		{name: "missing", code: "unauthorized", status: 401},
		{name: "malformed-token", token: "Bearer  verified", code: "unauthorized", status: 401},
		{name: "invalid", token: "Bearer invalid", code: "unauthorized", status: 401},
		{name: "empty-uid", token: "Bearer verified", code: "unauthorized", status: 401, alter: func(d *Dependencies, _ map[string]any) {
			d.Identity = verifier(func(context.Context, string) (Identity, error) { return Identity{}, nil })
		}},
		{name: "anonymous", token: "Bearer verified", code: "anonymous_identity", status: 403, alter: func(d *Dependencies, _ map[string]any) {
			d.Identity = verifier(func(context.Context, string) (Identity, error) {
				return Identity{UID: "anonymous", Anonymous: true}, nil
			})
		}},
		{name: "identity-absent", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) { d.Identity = nil }},
		{name: "identity-unavailable", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) {
			d.Identity = verifier(func(context.Context, string) (Identity, error) { return Identity{}, ErrUnavailable })
		}},
		{name: "media", token: "Bearer verified", media: "text/plain", code: "unsupported_media_type", status: 415},
		{name: "bad-media", token: "Bearer verified", media: ";", code: "unsupported_media_type", status: 415},
		{name: "oversize", token: "Bearer verified", source: strings.Repeat(" ", snapshot.MaxBytes+1), code: "body_too_large", status: 413},
		{name: "json", token: "Bearer verified", source: `{"snapshot":`, code: "invalid_json", status: 400},
		{name: "duplicate", token: "Bearer verified", source: `{"snapshot":{},"snapshot":{}}`, code: "invalid_json", status: 400},
		{name: "root-array", token: "Bearer verified", source: `[]`, code: "invalid_request", status: 400},
		{name: "case-alias", token: "Bearer verified", code: "invalid_request", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["Snapshot"] = m["snapshot"]; delete(m, "snapshot") }},
		{name: "null-revision", token: "Bearer verified", code: "invalid_revision", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["expectedRevision"] = nil }},
		{name: "unknown-owner", token: "Bearer verified", code: "invalid_request", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["ownerUid"] = "caller-admin" }},
		{name: "wrong-type", token: "Bearer verified", code: "invalid_request", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["digest"] = 1 }},
		{name: "key", token: "Bearer verified", code: "invalid_request", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["idempotencyKey"] = "bad key" }},
		{name: "digest-syntax", token: "Bearer verified", code: "invalid_request", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["digest"] = "abc" }},
		{name: "digest-mismatch", token: "Bearer verified", code: "digest_mismatch", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["digest"] = strings.Repeat("0", 64) }},
		{name: "snapshot", token: "Bearer verified", code: "invalid_snapshot", status: 400, alter: func(_ *Dependencies, m map[string]any) {
			m["snapshot"] = map[string]any{"format": "querydock-fiddle/2"}
		}},
		{name: "create-revision", token: "Bearer verified", code: "invalid_revision", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["expectedRevision"] = 1 }},
		{name: "append-missing-revision", token: "Bearer verified", path: "/api/querydock/fiddles/" + fiddleID + "/revisions", code: "invalid_revision", status: 400},
		{name: "append-zero-revision", token: "Bearer verified", path: "/api/querydock/fiddles/" + fiddleID + "/revisions", code: "invalid_revision", status: 400, alter: func(_ *Dependencies, m map[string]any) { m["expectedRevision"] = 0 }},
		{name: "bad-id", token: "Bearer verified", path: "/api/querydock/fiddles/reserved/revisions", code: "invalid_id", status: 400},
		{name: "store-absent", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) { d.Store = nil }},
		{name: "clock-absent", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) { d.Clock = nil }},
		{name: "ids-absent", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) { d.IDs = nil }},
		{name: "id-error", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) {
			d.IDs = ids(func() (string, error) { return "", errors.New("random unavailable") })
		}},
		{name: "invalid-server-id", token: "Bearer verified", code: "unavailable", status: 503, alter: func(d *Dependencies, _ map[string]any) { d.IDs = ids(func() (string, error) { return "new", nil }) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, db, m := setup(t)
			if tc.alter != nil {
				tc.alter(&d, m)
			}
			source := tc.source
			if source == "" {
				source = body(t, m)
			}
			media := tc.media
			if media == "" {
				media = "application/json"
			}
			path := tc.path
			if path == "" {
				path = "/api/querydock/fiddles"
			}
			w := request(Handler(d), "POST", path, source, tc.token, media)
			expect(t, w, tc.status, tc.code)
			if len(db.writes) != 0 {
				t.Fatal("rejected write reached storage")
			}
		})
	}
}
func TestStorageErrorsAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{ErrForbidden, 403, "forbidden"}, {ErrNotFound, 404, "not_found"}, {ErrConflict, 409, "conflict"}, {ErrUnavailable, 503, "unavailable"}, {errors.New("private SQL token"), 500, "internal"}, {&LimitError{Code: "revisions_per_minute", RetryAfterSeconds: 60}, 429, "quota_exceeded"}} {
		d, db, m := setup(t)
		db.err = tc.err
		w := request(Handler(d), "POST", "/api/querydock/fiddles", body(t, m), "Bearer verified", "application/json")
		expect(t, w, tc.status, tc.code)
		if len(db.writes) != 1 || strings.Contains(w.Body.String(), "private SQL token") {
			t.Fatal("storage/error boundary")
		}
		if tc.status == 429 {
			if w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "revisions_per_minute") {
				t.Fatal("quota guidance missing")
			}
			if tc.err.Error() != "revisions_per_minute" {
				t.Fatal("limit error unstable")
			}
		}
	}
	d, db, m := setup(t)
	db.panicValue = "secret panic SQL"
	w := request(Handler(d), "POST", "/api/querydock/fiddles", body(t, m), "Bearer verified", "application/json")
	expect(t, w, 500, "internal")
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("panic leaked")
	}
	for _, alter := range []func(*Receipt){func(r *Receipt) { r.ID = "new" }, func(r *Receipt) { r.Revision = 0 }, func(r *Receipt) { r.Digest = strings.Repeat("0", 64) }} {
		d, db, m := setup(t)
		alter(&db.receipt)
		expect(t, request(Handler(d), "POST", "/api/querydock/fiddles", body(t, m), "Bearer verified", "application/json"), 500, "internal")
	}
}
func TestPublicReadsAndDelete(t *testing.T) {
	for _, path := range []string{"/api/querydock/fiddles/" + fiddleID, "/api/querydock/fiddles/" + fiddleID + "/revisions/1"} {
		d, db, _ := setup(t)
		w := request(Handler(d), "GET", path, "", "", "")
		expect(t, w, 200, "")
		if db.reads != 1 || strings.Contains(w.Body.String(), "owner") {
			t.Fatal("public identity leak")
		}
		var reply struct {
			Snapshot json.RawMessage
			Digest   string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		s, err := snapshot.Import(reply.Snapshot)
		if err != nil || s != fixture(t) || reply.Digest != s.Digest {
			t.Fatal("public snapshot changed")
		}
	}
	for _, path := range []string{"/api/querydock/fiddles/new", "/api/querydock/fiddles/" + fiddleID + "/revisions/0", "/api/querydock/fiddles/" + fiddleID + "/revisions/01", "/api/querydock/fiddles/" + fiddleID + "/revisions/no", "/api/querydock/fiddles/" + fiddleID + "/revisions/18446744073709551616", "/api/querydock/fiddles"} {
		d, db, _ := setup(t)
		expect(t, request(Handler(d), "GET", path, "", "", ""), 404, "not_found")
		if db.reads != 0 {
			t.Fatal("invalid route reached storage")
		}
	}
	for _, kind := range []string{"absent", "deleted", "bad-snapshot", "bad-id", "zero-revision", "wrong-revision", "bad-digest"} {
		d, db, _ := setup(t)
		switch kind {
		case "absent":
			d.Store = nil
		case "deleted":
			db.err = ErrNotFound
		case "bad-snapshot":
			db.saved.Snapshot.JSON = `{}`
		case "bad-id":
			db.saved.ID = "wrong"
		case "zero-revision":
			db.saved.Revision = 0
		case "wrong-revision":
			db.saved.Revision = 2
		case "bad-digest":
			db.saved.Snapshot.Digest = "wrong"
		}
		status, code := 500, "internal"
		if kind == "absent" {
			status, code = 503, "unavailable"
		}
		if kind == "deleted" {
			status, code = 404, "not_found"
		}
		expect(t, request(Handler(d), "GET", "/api/querydock/fiddles/"+fiddleID+"/revisions/1", "", "", ""), status, code)
	}
	for _, kind := range []string{"success", "auth", "id", "store", "clock", "forbidden"} {
		d, db, _ := setup(t)
		path, token := "/api/querydock/fiddles/"+fiddleID, "Bearer verified"
		status, code := 204, ""
		switch kind {
		case "auth":
			token = ""
			status, code = 401, "unauthorized"
		case "id":
			path = "/api/querydock/fiddles/new"
			status, code = 400, "invalid_id"
		case "store":
			d.Store = nil
			status, code = 503, "unavailable"
		case "clock":
			d.Clock = nil
			status, code = 503, "unavailable"
		case "forbidden":
			db.err = ErrForbidden
			status, code = 403, "forbidden"
		}
		expect(t, request(Handler(d), "DELETE", path, "", token, ""), status, code)
		if kind == "success" {
			if len(db.deletes) != 1 || db.deletes[0].OwnerUID != "verified-owner" || db.deletes[0].At != d.Clock.Now() {
				t.Fatal("delete lost authority/clock")
			}
		} else if kind != "forbidden" && len(db.deletes) != 0 {
			t.Fatal("invalid delete reached storage")
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestBodyReadFailure(t *testing.T) {
	d, db, _ := setup(t)
	r := httptest.NewRequest("POST", "/api/querydock/fiddles", nil)
	r.Body = io.NopCloser(failingReader{})
	r.Header.Set("Authorization", "Bearer verified")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	Handler(d).ServeHTTP(w, r)
	expect(t, w, 400, "invalid_body")
	if len(db.writes) != 0 {
		t.Fatal("unreadable body persisted")
	}
}
func TestRuntimePorts(t *testing.T) {
	raw := bytes.Repeat([]byte{0xfb}, 16)
	id, err := (SecureIDs{Reader: bytes.NewReader(raw)}).NewID()
	if err != nil || len(id) != 22 || !idPattern.MatchString(id) {
		t.Fatalf("bad secure ID %q %v", id, err)
	}
	if _, err := (SecureIDs{Reader: bytes.NewReader(raw[:2])}).NewID(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("random failure ignored")
	}
	before := time.Now()
	at := (SystemClock{}).Now()
	if at.Location() != time.UTC || at.Before(before) || at.After(time.Now()) {
		t.Fatal("clock isn't current UTC")
	}
}
