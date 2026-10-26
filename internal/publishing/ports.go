// Package publishing defines the HTTP/storage boundary, never a SQL executor.
package publishing

import (
	"context"
	"errors"
	"time"

	"github.com/datatug/querydock-go/internal/snapshot"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnavailable  = errors.New("adapters unavailable")
)

// Identity is emitted only by a cryptographically verifying adapter. Request
// JSON, headers other than the bearer credential, and caller claims are not identity.
type Identity struct {
	UID       string
	Anonymous bool
}
type IdentityVerifier interface {
	Verify(context.Context, string) (Identity, error)
}
type Clock interface{ Now() time.Time }
type IDSource interface{ NewID() (string, error) }

// Write carries server-derived authority and a detached, recomputed snapshot.
// The storage adapter MUST check ownership, tombstones, expected revision,
// idempotency (owner/key/body) and quotas atomically, before any durable write.
// Task 1 defines this port; no durable or volatile production adapter exists yet.
type Write struct {
	OwnerUID         string
	ID               string
	ExpectedRevision uint64
	IdempotencyKey   string
	Snapshot         snapshot.Validated
	At               time.Time
}
type Receipt struct {
	ID           string `json:"id"`
	Revision     uint64 `json:"revision"`
	Digest       string `json:"digest"`
	CanonicalURL string `json:"canonicalUrl"`
}

// PublicRevision deliberately has no owner, identity or internal quota fields.
type PublicRevision struct {
	ID       string
	Revision uint64
	Snapshot snapshot.Validated
}
type Delete struct {
	OwnerUID string
	ID       string
	At       time.Time
}
type Store interface {
	Create(context.Context, Write) (Receipt, error)
	Append(context.Context, Write) (Receipt, error)
	Read(context.Context, string, uint64) (PublicRevision, error) // revision 0 resolves latest.
	Delete(context.Context, Delete) error
}

// LimitError gives stable quota-specific retry guidance. Later transaction
// adapters use the approved limits; the HTTP boundary never counts locally.
type LimitError struct {
	Code              string
	RetryAfterSeconds uint64
}

func (e *LimitError) Error() string { return e.Code }

type Dependencies struct {
	Identity IdentityVerifier
	Clock    Clock
	IDs      IDSource
	Store    Store
}
