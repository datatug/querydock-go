package publishing

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/datatug/querydock-go/internal/snapshot"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type boundary struct{ deps Dependencies }
type mutation struct {
	Snapshot         json.RawMessage `json:"snapshot"`
	Digest           string          `json:"digest"`
	IdempotencyKey   string          `json:"idempotencyKey"`
	ExpectedRevision *uint64         `json:"expectedRevision,omitempty"`
}
type errorBody struct {
	Error errorDetail `json:"error"`
}
type errorDetail struct {
	Code              string `json:"code"`
	Limit             string `json:"limit,omitempty"`
	RetryAfterSeconds uint64 `json:"retryAfterSeconds,omitempty"`
}

// Handler always fails closed when an adapter is absent. Its exact routes expose
// no list endpoint. Recovery emits no panic, token, source SQL or internal error.
func Handler(deps Dependencies) http.Handler {
	b := boundary{deps}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/querydock/fiddles", b.create)
	mux.HandleFunc("POST /api/querydock/fiddles/{id}/revisions", b.append)
	mux.HandleFunc("GET /api/querydock/fiddles/{id}", b.read)
	mux.HandleFunc("GET /api/querydock/fiddles/{id}/revisions/{revision}", b.read)
	mux.HandleFunc("DELETE /api/querydock/fiddles/{id}", b.delete)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fail(w, http.StatusNotFound, "not_found") })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if recover() != nil {
				fail(w, http.StatusInternalServerError, "internal")
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

func (b boundary) identity(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	parts := strings.Split(r.Header.Get("Authorization"), " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		fail(w, 401, "unauthorized")
		return Identity{}, false
	}
	if b.deps.Identity == nil {
		fail(w, 503, "unavailable")
		return Identity{}, false
	}
	identity, err := b.deps.Identity.Verify(r.Context(), parts[1])
	if errors.Is(err, ErrUnavailable) {
		fail(w, 503, "unavailable")
		return Identity{}, false
	}
	if err != nil || identity.UID == "" {
		fail(w, 401, "unauthorized")
		return Identity{}, false
	}
	if identity.Anonymous {
		fail(w, 403, "anonymous_identity")
		return Identity{}, false
	}
	return identity, true
}

func (b boundary) create(w http.ResponseWriter, r *http.Request) { b.write(w, r, false) }
func (b boundary) append(w http.ResponseWriter, r *http.Request) { b.write(w, r, true) }
func (b boundary) write(w http.ResponseWriter, r *http.Request, appendRevision bool) {
	identity, ok := b.identity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if appendRevision && !idPattern.MatchString(id) {
		fail(w, 400, "invalid_id")
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		fail(w, 415, "unsupported_media_type")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, snapshot.MaxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "body_too_large")
		} else {
			fail(w, 400, "invalid_body")
		}
		return
	}
	parsed, parseErr := snapshot.ParseJSON(body)
	if parseErr != nil {
		fail(w, 400, "invalid_json")
		return
	}
	fields, ok := parsed.(map[string]any)
	if !ok {
		fail(w, 400, "invalid_request")
		return
	}
	for key := range fields {
		switch key {
		case "snapshot", "digest", "idempotencyKey", "expectedRevision":
		default:
			fail(w, 400, "invalid_request")
			return
		}
	}
	var input mutation
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || !keyPattern.MatchString(input.IdempotencyKey) || !digestPattern.MatchString(input.Digest) {
		fail(w, 400, "invalid_request")
		return
	}
	_, hasRevision := fields["expectedRevision"]
	if appendRevision != hasRevision || (hasRevision && (input.ExpectedRevision == nil || *input.ExpectedRevision == 0)) {
		fail(w, 400, "invalid_revision")
		return
	}
	validated, err := snapshot.Import(input.Snapshot)
	if err != nil {
		fail(w, 400, "invalid_snapshot")
		return
	}
	if input.Digest != validated.Digest {
		fail(w, 400, "digest_mismatch")
		return
	}
	if b.deps.Store == nil || b.deps.Clock == nil || b.deps.IDs == nil {
		fail(w, 503, "unavailable")
		return
	}
	if !appendRevision {
		id, err = b.deps.IDs.NewID()
		if err != nil || !idPattern.MatchString(id) {
			fail(w, 503, "unavailable")
			return
		}
	}
	write := Write{OwnerUID: identity.UID, ID: id, IdempotencyKey: input.IdempotencyKey, Snapshot: validated, At: b.deps.Clock.Now()}
	var receipt Receipt
	if appendRevision {
		write.ExpectedRevision = *input.ExpectedRevision
		receipt, err = b.deps.Store.Append(r.Context(), write)
	} else {
		receipt, err = b.deps.Store.Create(r.Context(), write)
	}
	if err != nil {
		storeError(w, err)
		return
	}
	// An idempotent create retry may return the original ID rather than the new
	// candidate. Only the trusted atomic adapter can decide which receipt wins.
	if !idPattern.MatchString(receipt.ID) || receipt.Revision == 0 || receipt.Digest != validated.Digest {
		fail(w, 500, "internal")
		return
	}
	receipt.CanonicalURL = "https://querydock.run/sql/" + receipt.ID + "/" + strconv.FormatUint(receipt.Revision, 10)
	respond(w, 200, receipt)
}

func (b boundary) read(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		fail(w, 404, "not_found")
		return
	}
	var revision uint64
	if text := r.PathValue("revision"); text != "" {
		var err error
		revision, err = strconv.ParseUint(text, 10, 64)
		if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != text {
			fail(w, 404, "not_found")
			return
		}
	}
	if b.deps.Store == nil {
		fail(w, 503, "unavailable")
		return
	}
	saved, err := b.deps.Store.Read(r.Context(), id, revision)
	if err != nil {
		storeError(w, err)
		return
	}
	validated, err := snapshot.Import([]byte(saved.Snapshot.JSON))
	if err != nil || saved.ID != id || saved.Revision == 0 || (revision != 0 && saved.Revision != revision) || validated.Digest != saved.Snapshot.Digest {
		fail(w, 500, "internal")
		return
	}
	respond(w, 200, struct {
		ID       string          `json:"id"`
		Revision uint64          `json:"revision"`
		Digest   string          `json:"digest"`
		Snapshot json.RawMessage `json:"snapshot"`
	}{saved.ID, saved.Revision, validated.Digest, json.RawMessage(validated.JSON)})
}

func (b boundary) delete(w http.ResponseWriter, r *http.Request) {
	identity, ok := b.identity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		fail(w, 400, "invalid_id")
		return
	}
	if b.deps.Store == nil || b.deps.Clock == nil {
		fail(w, 503, "unavailable")
		return
	}
	if err := b.deps.Store.Delete(r.Context(), Delete{OwnerUID: identity.UID, ID: id, At: b.deps.Clock.Now()}); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func storeError(w http.ResponseWriter, err error) {
	var limit *LimitError
	switch {
	case errors.As(err, &limit):
		w.Header().Set("Retry-After", strconv.FormatUint(limit.RetryAfterSeconds, 10))
		respond(w, 429, errorBody{errorDetail{Code: "quota_exceeded", Limit: limit.Code, RetryAfterSeconds: limit.RetryAfterSeconds}})
	case errors.Is(err, ErrForbidden):
		fail(w, 403, "forbidden")
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "not_found")
	case errors.Is(err, ErrConflict):
		fail(w, 409, "conflict")
	case errors.Is(err, ErrUnavailable):
		fail(w, 503, "unavailable")
	default:
		fail(w, 500, "internal")
	}
}
func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, errorBody{errorDetail{Code: code}})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Only fixed response DTOs reach this function. A write failure means the
	// connection has gone away; HTTP cannot send a second response to that client.
	_ = json.NewEncoder(w).Encode(value)
}
