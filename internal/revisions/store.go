// Package revisions implements immutable, owner-checked DALgo transactions.
// Production remains gated until the separately verified live deployment.
package revisions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/snapshot"
	"github.com/sneat-co/sneat-go-core/firestorepolicy"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var validKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Store struct{ db dal.DB }

// New always layers the owning Core root policy over DALgo, preserving existing
// guards. It never accepts a caller-provided registry or relaxes a policy.
func New(db dal.DB) (*Store, error) {
	secured, err := firestorepolicy.ProtectRootCollections(db)
	if err != nil {
		return nil, err
	}
	return &Store{db: secured}, nil
}

type fiddle struct {
	OwnerUID       string    `firestore:"ownerUid"`
	Latest         int64     `firestore:"latest"`
	Deleted        bool      `firestore:"deleted"`
	CreatedAt      time.Time `firestore:"createdAt"`
	UpdatedAt      time.Time `firestore:"updatedAt"`
	Bytes          int64     `firestore:"bytes"`
	DeleteRevision int64     `firestore:"deleteRevision"`
	DeleteBytes    int64     `firestore:"deleteBytes"`
	CleanupNext    int64     `firestore:"cleanupNext"`
	CleanupDone    bool      `firestore:"cleanupDone"`
}
type revision struct {
	Number    int64     `firestore:"revision"`
	JSON      string    `firestore:"snapshotJson"`
	Digest    string    `firestore:"digest"`
	CreatedAt time.Time `firestore:"createdAt"`
}
type ledger struct {
	OwnerUID    string `firestore:"ownerUid"`
	Fingerprint string `firestore:"fingerprint"`
	ID          string `firestore:"id"`
	Revision    int64  `firestore:"revision"`
	Digest      string `firestore:"digest"`
}

var extension = record.NewKeyWithID("ext", "querydock")

func fiddleKey(id string) *record.Key { return record.NewKeyWithParentAndID(extension, "fiddles", id) }
func revisionKey(id string, n int64) *record.Key {
	return record.NewKeyWithParentAndID(fiddleKey(id), "revisions", strconv.FormatInt(n, 10))
}
func hashFields(fields ...string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
func ledgerKey(owner, key string) *record.Key {
	return record.NewKeyWithParentAndID(extension, "idempotency", hashFields(owner, key))
}
func get(ctx context.Context, tx dal.Getter, r record.Record) (bool, error) {
	err := tx.Get(ctx, r)
	if record.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.Exists(), nil
}
func validWrite(w publishing.Write, appendRevision bool) bool {
	if w.OwnerUID == "" || len(w.OwnerUID) > 128 || !utf8.ValidString(w.OwnerUID) || w.At.IsZero() || !validID.MatchString(w.ID) || !validKey.MatchString(w.IdempotencyKey) || appendRevision != (w.ExpectedRevision > 0) || w.ExpectedRevision >= math.MaxInt64 {
		return false
	}
	validated, err := snapshot.Import([]byte(w.Snapshot.JSON))
	return err == nil && validated == w.Snapshot
}
func (s *Store) Create(ctx context.Context, w publishing.Write) (publishing.Receipt, error) {
	return s.write(ctx, w, false)
}
func (s *Store) Append(ctx context.Context, w publishing.Write) (publishing.Receipt, error) {
	return s.write(ctx, w, true)
}
func (s *Store) write(ctx context.Context, w publishing.Write, appendRevision bool) (publishing.Receipt, error) {
	if !validWrite(w, appendRevision) {
		return publishing.Receipt{}, publishing.ErrConflict
	}
	operation, target := "create", ""
	if appendRevision {
		operation, target = "append", w.ID
	}
	fingerprint := hashFields("querydock-write/1", w.OwnerUID, operation, target, strconv.FormatUint(w.ExpectedRevision, 10), w.Snapshot.JSON)
	var receipt publishing.Receipt
	err := s.db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		// Every mutable record is fresh on each Firestore callback retry. Random ID,
		// timestamp and fingerprint stay fixed outside the callback.
		receipt = publishing.Receipt{}
		saved := ledger{}
		savedRecord := record.NewRecordWithData(ledgerKey(w.OwnerUID, w.IdempotencyKey), &saved)
		exists, err := get(ctx, tx, savedRecord)
		if err != nil {
			return err
		}
		if exists {
			if saved.OwnerUID != w.OwnerUID || saved.Fingerprint != fingerprint {
				return publishing.ErrConflict
			}
			if !validID.MatchString(saved.ID) || saved.Revision < 1 || saved.Digest != w.Snapshot.Digest || (appendRevision && (saved.ID != w.ID || uint64(saved.Revision) != w.ExpectedRevision+1)) {
				return publishing.ErrUnavailable
			}
			state := fiddle{}
			stateRecord := record.NewRecordWithData(fiddleKey(saved.ID), &state)
			found, err := get(ctx, tx, stateRecord)
			if err != nil {
				return err
			}
			if !found || state.Deleted {
				return publishing.ErrNotFound
			}
			if state.OwnerUID != w.OwnerUID {
				return publishing.ErrForbidden
			}
			if state.Latest < saved.Revision {
				return publishing.ErrUnavailable
			}
			receipt = makeReceipt(saved.ID, saved.Revision, saved.Digest)
			return nil
		}
		state := fiddle{}
		stateRecord := record.NewRecordWithData(fiddleKey(w.ID), &state)
		exists, err = get(ctx, tx, stateRecord)
		if err != nil {
			return err
		}
		next := int64(1)
		if appendRevision {
			if !exists || state.Deleted {
				return publishing.ErrNotFound
			}
			if state.OwnerUID != w.OwnerUID {
				return publishing.ErrForbidden
			}
			if state.Latest < 1 || uint64(state.Latest) != w.ExpectedRevision {
				return publishing.ErrConflict
			}
			if state.Latest >= MaxRevisions {
				return &publishing.LimitError{Code: LimitRevisions, Action: "fork_fiddle"}
			}
			next = state.Latest + 1
		} else {
			if exists {
				return publishing.ErrConflict
			}
			state = fiddle{OwnerUID: w.OwnerUID, CreatedAt: w.At}
		}
		history := revision{Number: next, JSON: w.Snapshot.JSON, Digest: w.Snapshot.Digest, CreatedAt: w.At}
		historyRecord := record.NewRecordWithData(revisionKey(w.ID, next), &history)
		exists, err = get(ctx, tx, historyRecord)
		if err != nil {
			return err
		}
		if exists {
			return publishing.ErrConflict
		}
		counters := usage{}
		countersRecord := record.NewRecordWithData(usageKey(w.OwnerUID), &counters)
		found, err := get(ctx, tx, countersRecord)
		if err != nil {
			return err
		}
		if !found {
			counters = usage{OwnerUID: w.OwnerUID}
		}
		if err := charge(&counters, state, w, appendRevision); err != nil {
			return err
		}
		state.Bytes += int64(len(w.Snapshot.JSON))
		state.Latest, state.UpdatedAt = next, w.At
		saved = ledger{OwnerUID: w.OwnerUID, Fingerprint: fingerprint, ID: w.ID, Revision: next, Digest: w.Snapshot.Digest}
		if appendRevision {
			err = tx.Set(ctx, stateRecord)
		} else {
			err = tx.Insert(ctx, stateRecord)
		}
		if err != nil {
			return err
		}
		if err = tx.Insert(ctx, historyRecord); err != nil {
			return err
		}
		if err = tx.Insert(ctx, savedRecord); err != nil {
			return err
		}
		if err = tx.Set(ctx, countersRecord); err != nil {
			return err
		}
		receipt = makeReceipt(w.ID, next, w.Snapshot.Digest)
		return nil
	})
	if err != nil {
		if errors.Is(err, record.ErrRecordExists) {
			return publishing.Receipt{}, publishing.ErrConflict
		}
		return publishing.Receipt{}, err
	}
	return receipt, nil
}
func makeReceipt(id string, revision int64, digest string) publishing.Receipt {
	return publishing.Receipt{ID: id, Revision: uint64(revision), Digest: digest, CanonicalURL: "https://querydock.run/sql/" + id + "/" + strconv.FormatInt(revision, 10)}
}
