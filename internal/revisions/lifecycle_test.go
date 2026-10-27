package revisions

import (
	"context"
	"errors"
	"fmt"
	"github.com/datatug/querydock-go/internal/publishing"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func seed(t *testing.T, n int) (*Store, *fakeDB, publishing.Write, publishing.Delete) {
	t.Helper()
	s, db, w := setup(t)
	for i := 1; i <= n; i++ {
		saveAppend(t, s, w, i, w.At)
	}
	w.IdempotencyKey = "revision-1"
	return s, db, w, publishing.Delete{OwnerUID: w.OwnerUID, ID: w.ID, At: w.At.Add(time.Second)}
}
func equalData(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatal("record count changed")
	}
	for k, v := range before {
		if string(after[k]) != string(v) {
			t.Fatalf("record changed: %s", k)
		}
	}
}
func TestPublicReadConsistencyAndValidation(t *testing.T) {
	s, db, w, _ := seed(t, 3)
	before := cloneData(db.data)
	for _, n := range []uint64{0, 1, 3} {
		result, err := s.Read(context.Background(), w.ID, n)
		want := n
		if n == 0 {
			want = 3
		}
		if err != nil || result.ID != w.ID || result.Revision != want || result.Snapshot != w.Snapshot {
			t.Fatalf("public read %+v %v", result, err)
		}
	}
	equalData(t, before, db.data)
	for _, tc := range []struct {
		id string
		n  uint64
	}{{"reserved", 1}, {w.ID, 101}, {w.ID, 4}, {otherID, 1}} {
		if _, err := s.Read(context.Background(), tc.id, tc.n); !errors.Is(err, publishing.ErrNotFound) {
			t.Fatal("invalid public reference accepted")
		}
	}
	for _, kind := range []string{"missing", "zero-latest", "many-latest", "json", "noncanonical", "digest", "number", "state-read", "payload-read", "transaction"} {
		t.Run(kind, func(t *testing.T) {
			s, db, w, _ := seed(t, 1)
			state := load[fiddle](t, db, fiddleKey(w.ID))
			history := load[revision](t, db, revisionKey(w.ID, 1))
			want := publishing.ErrUnavailable
			switch kind {
			case "missing":
				delete(db.data, revisionKey(w.ID, 1).String())
				want = publishing.ErrNotFound
			case "zero-latest":
				state.Latest = 0
			case "many-latest":
				state.Latest = 101
			case "json":
				history.JSON = `{}`
			case "noncanonical":
				history.JSON = " " + history.JSON
			case "digest":
				history.Digest = "wrong"
			case "number":
				history.Number = 2
			case "state-read":
				db.faultOp, db.faultAt = "get", 1
				want = errInjected
			case "payload-read":
				db.faultOp, db.faultAt = "get", 2
				want = errInjected
			case "transaction":
				db.runErr = errInjected
				want = errInjected
			}
			put(t, db, fiddleKey(w.ID), state)
			if kind != "missing" {
				put(t, db, revisionKey(w.ID, 1), history)
			}
			result, err := s.Read(context.Background(), w.ID, 1)
			if !errors.Is(err, want) || result != (publishing.PublicRevision{}) {
				t.Fatalf("read %+v %v want%v", result, err, want)
			}
		})
	}
}
func TestDeletionStagesReserveUntilFinalization(t *testing.T) {
	s, db, w, d := seed(t, 3)
	u := load[usage](t, db, usageKey(w.OwnerUID))
	if err := s.tombstone(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	state := load[fiddle](t, db, fiddleKey(w.ID))
	if !state.Deleted || state.DeleteRevision != 3 || state.DeleteBytes != u.Bytes || state.CleanupNext != 1 {
		t.Fatalf("wrong tombstone %+v", state)
	}
	if _, err := s.Read(context.Background(), w.ID, 0); !errors.Is(err, publishing.ErrNotFound) {
		t.Fatal("tombstone remained public")
	}
	if _, err := s.Create(context.Background(), w); !errors.Is(err, publishing.ErrNotFound) {
		t.Fatal("save replay resurrected tombstone")
	}
	changedBody := w
	changedBody.Snapshot = changed(t, w.Snapshot)
	if _, err := s.Create(context.Background(), changedBody); !errors.Is(err, publishing.ErrConflict) {
		t.Fatal("changed-key reuse lost its conflict")
	}
	appendWrite := w
	appendWrite.ExpectedRevision = 3
	appendWrite.IdempotencyKey = "new-append"
	if _, err := s.Append(context.Background(), appendWrite); !errors.Is(err, publishing.ErrNotFound) {
		t.Fatal("tombstone allowed append")
	}
	for n := 1; n <= 3; n++ {
		done, err := s.cleanupStep(context.Background(), d)
		if err != nil || done {
			t.Fatalf("cleanup step %d done%v %v", n, done, err)
		}
		if _, exists := db.data[revisionKey(w.ID, int64(n)).String()]; exists {
			t.Fatal("payload retained after committed cleanup")
		}
		if got := load[usage](t, db, usageKey(w.OwnerUID)); !reflect.DeepEqual(got, u) {
			t.Fatal("cleanup released reservations or refunded rate before finalization")
		}
	}
	done, err := s.cleanupStep(context.Background(), d)
	if err != nil || !done {
		t.Fatalf("finalization %v %v", done, err)
	}
	after := load[usage](t, db, usageKey(w.OwnerUID))
	if after.Bytes != 0 || after.Fiddles != 0 || !reflect.DeepEqual(after.Revisions, u.Revisions) {
		t.Fatalf("wrong release %+v", after)
	}
	for i := 0; i < 3; i++ {
		if err := s.Delete(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if got := load[usage](t, db, usageKey(w.OwnerUID)); !reflect.DeepEqual(got, after) {
		t.Fatal("delete retry double-released quota")
	}
	for _, n := range []uint64{0, 1, 3} {
		if _, err := s.Read(context.Background(), w.ID, n); !errors.Is(err, publishing.ErrNotFound) {
			t.Fatal("deleted revision remained public")
		}
	}
	w.ID = otherID
	w.IdempotencyKey = "fresh-fiddle"
	if _, err := s.Create(context.Background(), w); err != nil {
		t.Fatal("completed cleanup failed to free reservations")
	}
}
func TestDeletionCrashAndUncertainCommitRecovery(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		for _, offset := range []int{1, 2, 3, 5} {
			t.Run(fmt.Sprintf("uncertain%v-step%d", uncertain, offset), func(t *testing.T) {
				s, db, w, d := seed(t, 3)
				initial := load[usage](t, db, usageKey(w.OwnerUID))
				at := db.runs + offset
				if uncertain {
					db.afterCommitAt, db.afterCommitErr = at, errInjected
				} else {
					db.failRunAt, db.runErr = at, errInjected
				}
				if err := s.Delete(context.Background(), d); !errors.Is(err, publishing.ErrUnavailable) {
					t.Fatalf("interruption returned success: %v", err)
				}
				state := load[fiddle](t, db, fiddleKey(w.ID))
				if offset > 1 || uncertain {
					if !state.Deleted {
						t.Fatal("committed tombstone was lost")
					}
					if _, err := s.Read(context.Background(), w.ID, 1); !errors.Is(err, publishing.ErrNotFound) && (db.runErr == nil || !errors.Is(err, errInjected)) {
						t.Fatal("pending deletion remained public")
					}
				}
				current := load[usage](t, db, usageKey(w.OwnerUID))
				if uncertain && offset == 5 {
					if current.Fiddles != 0 || current.Bytes != 0 {
						t.Fatal("uncertain final commit lost release")
					}
				} else if !reflect.DeepEqual(current, initial) {
					t.Fatal("interrupted cleanup released quota")
				}
				db.runErr = nil
				db.afterCommitErr = nil
				if err := s.Delete(context.Background(), d); err != nil {
					t.Fatal(err)
				}
				final := load[usage](t, db, usageKey(w.OwnerUID))
				if final.Fiddles != 0 || final.Bytes != 0 || !reflect.DeepEqual(final.Revisions, initial.Revisions) {
					t.Fatal("resume leaked/refunded quota")
				}
				if !load[fiddle](t, db, fiddleKey(w.ID)).CleanupDone {
					t.Fatal("resume didn't finalize")
				}
			})
		}
	}
}
func TestMissingAlreadyCleanedPayloadAndFailures(t *testing.T) {
	s, db, w, d := seed(t, 2)
	if err := s.tombstone(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	delete(db.data, revisionKey(w.ID, 1).String())
	if err := s.Delete(context.Background(), d); err != nil {
		t.Fatal("missing already-cleaned payload blocked resume", err)
	}
	for _, op := range []string{"get-state", "get-payload", "delete", "set-progress", "get-final", "set-counter", "set-final"} {
		t.Run(op, func(t *testing.T) {
			s, db, w, d := seed(t, 1)
			if err := s.tombstone(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(op, "final") || op == "set-counter" {
				if _, err := s.cleanupStep(context.Background(), d); err != nil {
					t.Fatal(err)
				}
			}
			switch op {
			case "get-state":
				db.faultOp, db.faultAt = "get", 1
			case "get-payload", "get-final":
				db.faultOp, db.faultAt = "get", 2
			case "delete":
				db.faultOp, db.faultAt = "delete", 1
			case "set-progress", "set-counter":
				db.faultOp, db.faultAt = "set", 1
			case "set-final":
				db.faultOp, db.faultAt = "set", 2
			}
			before := cloneData(db.data)
			done, err := s.cleanupStep(context.Background(), d)
			if !errors.Is(err, errInjected) || done {
				t.Fatalf("failure %v %v", done, err)
			}
			equalData(t, before, db.data)
			if load[usage](t, db, usageKey(w.OwnerUID)).Fiddles != 1 {
				t.Fatal("failed cleanup refunded slot")
			}
		})
	}
}
func TestDeleteAuthorizationAndCorruptProgress(t *testing.T) {
	for _, kind := range []string{"bad-id", "missing", "owner", "empty-owner", "zero-latest", "many-latest", "zero-bytes", "many-bytes", "deleted-invalid", "get-error", "set-error", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s, db, w, d := seed(t, 1)
			state := load[fiddle](t, db, fiddleKey(w.ID))
			want := publishing.ErrUnavailable
			switch kind {
			case "bad-id":
				d.ID = "reserved"
				want = publishing.ErrNotFound
			case "missing":
				delete(db.data, fiddleKey(w.ID).String())
				want = publishing.ErrNotFound
			case "owner":
				d.OwnerUID = "another"
				want = publishing.ErrForbidden
			case "empty-owner":
				d.OwnerUID = ""
				want = publishing.ErrForbidden
			case "zero-latest":
				state.Latest = 0
			case "many-latest":
				state.Latest = 101
			case "zero-bytes":
				state.Bytes = 0
			case "many-bytes":
				state.Bytes = MaxRetainedBytes + 1
			case "deleted-invalid":
				state.Deleted = true
			case "get-error":
				db.faultOp, db.faultAt = "get", 1
			case "set-error":
				db.faultOp, db.faultAt = "set", 1
			}
			if kind != "missing" {
				put(t, db, fiddleKey(w.ID), state)
			}
			ctx := context.Background()
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := s.Delete(ctx, d); !errors.Is(err, want) {
				t.Fatalf("got %v want%v", err, want)
			}
		})
	}
	for _, kind := range []string{"missing", "owner", "live", "bad-progress", "done-before-drain", "missing-usage", "invalid-usage", "no-slot", "undercharged", "excess-final-bytes"} {
		t.Run("step-"+kind, func(t *testing.T) {
			s, db, w, d := seed(t, 1)
			if err := s.tombstone(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if _, err := s.cleanupStep(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			state := load[fiddle](t, db, fiddleKey(w.ID))
			u := load[usage](t, db, usageKey(w.OwnerUID))
			want := publishing.ErrUnavailable
			switch kind {
			case "missing":
				delete(db.data, fiddleKey(w.ID).String())
				want = publishing.ErrNotFound
			case "owner":
				d.OwnerUID = "other"
				want = publishing.ErrForbidden
			case "live":
				state.Deleted = false
			case "bad-progress":
				state.CleanupNext = 0
			case "done-before-drain":
				state.CleanupDone = true
				state.CleanupNext = 1
			case "missing-usage":
				delete(db.data, usageKey(w.OwnerUID).String())
			case "invalid-usage":
				u.OwnerUID = "other"
			case "no-slot":
				u.Fiddles = 0
				u.Bytes = 0
			case "undercharged":
				u.Bytes = 1
			case "excess-final-bytes":
				u.Bytes++
			}
			if kind != "missing" {
				put(t, db, fiddleKey(w.ID), state)
			}
			if kind != "missing-usage" {
				put(t, db, usageKey(w.OwnerUID), u)
			}
			done, err := s.cleanupStep(context.Background(), d)
			if !errors.Is(err, want) || done {
				t.Fatalf("step %v %v", done, err)
			}
		})
	}
}
func TestCompetingDeletersAndNoRateRefund(t *testing.T) {
	s, db, w, d := seed(t, 10)
	before := load[usage](t, db, usageKey(w.OwnerUID))
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(1)
	for i := 0; i < 2; i++ {
		go func() { ready.Wait(); results <- s.Delete(context.Background(), d) }()
	}
	ready.Done()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	after := load[usage](t, db, usageKey(w.OwnerUID))
	if after.Fiddles != 0 || after.Bytes != 0 || !reflect.DeepEqual(after.Revisions, before.Revisions) {
		t.Fatal("competing deleters double released or refunded rate")
	}
	w.ID = otherID
	w.IdempotencyKey = "after-deletion"
	_, err := s.Create(context.Background(), w)
	requireLimit(t, err, LimitRate, "wait")
}
func TestPendingReservationsAndBoundedNoProgress(t *testing.T) {
	s, db, w, d := seed(t, 1)
	if err := s.tombstone(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	u := load[usage](t, db, usageKey(w.OwnerUID))
	u.Fiddles = 100
	u.Bytes = MaxRetainedBytes
	put(t, db, usageKey(w.OwnerUID), u)
	attempt := w
	attempt.ID = otherID
	attempt.IdempotencyKey = "new"
	_, err := s.Create(context.Background(), attempt)
	requireLimit(t, err, LimitFiddles, "delete_fiddle")
	u.Fiddles = 2
	put(t, db, usageKey(w.OwnerUID), u)
	_, err = s.Create(context.Background(), attempt)
	requireLimit(t, err, LimitStorage, "delete_fiddle")
	db.dropCommit = true
	start := db.runs
	if err := s.Delete(context.Background(), d); !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("acknowledged no-progress adapter returned success")
	}
	if db.runs-start != MaxRevisions+2 {
		t.Fatal("cleanup request wasn't bounded")
	}
	db.dropCommit = false
	if err := s.Delete(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	after := load[usage](t, db, usageKey(w.OwnerUID))
	if after.Fiddles != 1 || after.Bytes != MaxRetainedBytes-int64(len(w.Snapshot.JSON)) {
		t.Fatal("cleanup released another fiddle's reservation")
	}
}
