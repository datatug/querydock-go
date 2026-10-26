package revisions

import (
	"context"
	"errors"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/snapshot"
	"time"
)

// Read uses one consistent read transaction for the tombstone/latest and payload.
// Neither owner data nor internal usage/idempotency records leave this boundary.
func (s *Store) Read(ctx context.Context, id string, requested uint64) (publishing.PublicRevision, error) {
	if !validID.MatchString(id) || requested > MaxRevisions {
		return publishing.PublicRevision{}, publishing.ErrNotFound
	}
	var result publishing.PublicRevision
	err := s.db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
		result = publishing.PublicRevision{}
		state := fiddle{}
		found, err := get(ctx, tx, record.NewRecordWithData(fiddleKey(id), &state))
		if err != nil {
			return err
		}
		if !found || state.Deleted {
			return publishing.ErrNotFound
		}
		if state.Latest < 1 || state.Latest > MaxRevisions {
			return publishing.ErrUnavailable
		}
		n := int64(requested)
		if n == 0 {
			n = state.Latest
		}
		if n > state.Latest {
			return publishing.ErrNotFound
		}
		data := revision{}
		found, err = get(ctx, tx, record.NewRecordWithData(revisionKey(id, n), &data))
		if err != nil {
			return err
		}
		if !found {
			return publishing.ErrNotFound
		}
		validated, err := snapshot.Import([]byte(data.JSON))
		if err != nil || validated.JSON != data.JSON || validated.Digest != data.Digest || data.Number != n {
			return publishing.ErrUnavailable
		}
		result = publishing.PublicRevision{ID: id, Revision: uint64(n), Snapshot: validated}
		return nil
	})
	if err != nil {
		return publishing.PublicRevision{}, err
	}
	return result, nil
}

func validTombstone(state fiddle) bool {
	return state.Latest >= 1 && state.Latest <= MaxRevisions && state.Bytes > 0 && state.Bytes <= MaxRetainedBytes && state.DeleteRevision == state.Latest && state.DeleteBytes == state.Bytes && state.CleanupNext >= 1 && state.CleanupNext <= state.DeleteRevision+1 && (!state.CleanupDone || state.CleanupNext == state.DeleteRevision+1)
}

// tombstone freezes the exact reservation and revision range before any payload
// deletion. No quota or rolling-rate reservation is released here.
func (s *Store) tombstone(ctx context.Context, d publishing.Delete) error {
	return s.db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		state := fiddle{}
		r := record.NewRecordWithData(fiddleKey(d.ID), &state)
		found, err := get(ctx, tx, r)
		if err != nil {
			return err
		}
		if !found {
			return publishing.ErrNotFound
		}
		if state.OwnerUID != d.OwnerUID {
			return publishing.ErrForbidden
		}
		if state.Deleted {
			if !validTombstone(state) {
				return publishing.ErrUnavailable
			}
			return nil
		}
		if state.Latest < 1 || state.Latest > MaxRevisions || state.Bytes < 1 || state.Bytes > MaxRetainedBytes {
			return publishing.ErrUnavailable
		}
		state.Deleted = true
		state.DeleteRevision = state.Latest
		state.DeleteBytes = state.Bytes
		state.CleanupNext = 1
		state.UpdatedAt = d.At
		return tx.Set(ctx, r)
	})
}

// cleanupStep deletes at most ONE payload and advances its cursor in the same
// transaction. A valid payload is <=512KiB; this avoids Firestore's 10MiB
// transaction limit even for the full 20MiB fiddle and its simple scalar indexes.
// Missing already-cleaned records are safe. Finalization never refunds rate.
func (s *Store) cleanupStep(ctx context.Context, d publishing.Delete) (bool, error) {
	done := false
	err := s.db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		done = false
		state := fiddle{}
		r := record.NewRecordWithData(fiddleKey(d.ID), &state)
		found, err := get(ctx, tx, r)
		if err != nil {
			return err
		}
		if !found {
			return publishing.ErrNotFound
		}
		if state.OwnerUID != d.OwnerUID {
			return publishing.ErrForbidden
		}
		if !state.Deleted || !validTombstone(state) {
			return publishing.ErrUnavailable
		}
		if state.CleanupDone {
			done = true
			return nil
		}
		if state.CleanupNext <= state.DeleteRevision {
			payload := record.NewRecordWithData(revisionKey(d.ID, state.CleanupNext), &revision{})
			exists, err := get(ctx, tx, payload)
			if err != nil {
				return err
			}
			if exists {
				if err := tx.Delete(ctx, payload.Key()); err != nil {
					return err
				}
			}
			state.CleanupNext++
			return tx.Set(ctx, r)
		}
		counters := usage{}
		counterRecord := record.NewRecordWithData(usageKey(d.OwnerUID), &counters)
		found, err = get(ctx, tx, counterRecord)
		if err != nil {
			return err
		}
		if !found || !validUsage(counters, d.OwnerUID) || counters.Fiddles < 1 || counters.Bytes < state.DeleteBytes {
			return publishing.ErrUnavailable
		}
		counters.Fiddles--
		counters.Bytes -= state.DeleteBytes
		if !validUsage(counters, d.OwnerUID) {
			return publishing.ErrUnavailable
		}
		state.CleanupDone = true
		if err := tx.Set(ctx, counterRecord); err != nil {
			return err
		}
		if err := tx.Set(ctx, r); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done && err == nil, err
}

// Delete is owner-addressable after public revocation. It returns success only
// after confirmed cleanup/finalization; an interrupted owner retry resumes it.
// There is no background erasure claim or uncharged retained-payload fallback.
func (s *Store) Delete(ctx context.Context, d publishing.Delete) error {
	if !validID.MatchString(d.ID) {
		return publishing.ErrNotFound
	}
	if d.OwnerUID == "" {
		return publishing.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.tombstone(ctx, d); err != nil {
		return deletionError(err)
	}
	for step := 0; step <= MaxRevisions; step++ {
		done, err := s.cleanupStep(ctx, d)
		if err != nil {
			return deletionError(err)
		}
		if done {
			return nil
		}
	}
	return publishing.ErrUnavailable
}
func deletionError(err error) error {
	if errors.Is(err, publishing.ErrNotFound) || errors.Is(err, publishing.ErrForbidden) {
		return err
	}
	return publishing.ErrUnavailable
}
