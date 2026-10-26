package revisions

import (
	"cloud.google.com/go/firestore"
	"context"
	"errors"
	"fmt"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2firestore"
	"github.com/dal-go/record"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/snapshot"
	"google.golang.org/api/option"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// SDK failures can arrive after the server committed. This test decorator never
// changes policy or persistence semantics; it only drops one acknowledgement.
type interruptedDB struct {
	dal.DB
	mu                           sync.Mutex
	calls, failBefore, failAfter int
	dropFinal, finalDropped      bool
}

func (d *interruptedDB) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, o ...dal.TransactionOption) error {
	d.mu.Lock()
	d.calls++
	n := d.calls
	before, after := d.failBefore, d.failAfter
	d.mu.Unlock()
	if n == before {
		return errInjected
	}
	final := false
	err := d.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		final = false
		return f(ctx, &observeFinalTx{ReadwriteTransaction: tx, final: &final})
	}, o...)
	d.mu.Lock()
	drop := err == nil && final && d.dropFinal
	if drop {
		d.dropFinal = false
		d.finalDropped = true
	}
	d.mu.Unlock()
	if drop {
		return errInjected
	}
	if err == nil && n == after {
		return errInjected
	}
	return err
}

type observeFinalTx struct {
	dal.ReadwriteTransaction
	final *bool
}

func (t *observeFinalTx) Set(ctx context.Context, r record.Record) error {
	if state, ok := r.Data().(*fiddle); ok && state.CleanupDone {
		*t.final = true
	}
	return t.ReadwriteTransaction.Set(ctx, r)
}
func TestFirestoreTask3Emulator(t *testing.T) {
	project := os.Getenv("QUERYDOCK_TEST_PROJECT")
	if project == "" {
		t.Skip("run scripts/sdk-emulators.py")
	}
	host, _, err := net.SplitHostPort(os.Getenv("FIRESTORE_EMULATOR_HOST"))
	if err != nil || host != "127.0.0.1" || !strings.HasPrefix(project, "demo-querydock-") {
		t.Fatal("refusing non-demo storage target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	client, err := firestore.NewClientWithDatabase(ctx, project, "(default)", option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	}()
	raw := dalgo2firestore.NewDatabase(project, client)
	s, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, _, base := setup(t)
	write := func(owner, id, key string) publishing.Write {
		w := base
		w.OwnerUID = owner
		w.ID = id
		w.IdempotencyKey = key
		return w
	}
	loadUsage := func(owner string) usage {
		t.Helper()
		doc, err := client.Doc(usageKey(owner).String()).Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var u usage
		if err := doc.DataTo(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	seedUsage := func(owner string, u usage) {
		t.Helper()
		if _, err := client.Doc(usageKey(owner).String()).Set(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct {
		receipt publishing.Receipt
		err     error
	}
	raceCreates := func(a, b publishing.Write) []outcome {
		t.Helper()
		start := make(chan struct{})
		results := make(chan outcome, 2)
		for _, w := range []publishing.Write{a, b} {
			go func(w publishing.Write) { <-start; r, e := s.Create(ctx, w); results <- outcome{r, e} }(w)
		}
		close(start)
		return []outcome{<-results, <-results}
	}
	requireRaceLimit := func(results []outcome, code string) {
		t.Helper()
		success, limited := 0, 0
		for _, r := range results {
			if r.err == nil {
				success++
			} else {
				var e *publishing.LimitError
				if !errors.As(r.err, &e) || e.Code != code {
					t.Fatalf("unexpected race error %v", r.err)
				}
				limited++
			}
		}
		if success != 1 || limited != 1 {
			t.Fatalf("race results %+v", results)
		}
	}
	t.Run("rolling-rate-race", func(t *testing.T) {
		owner := "task3-rate-owner"
		u := usage{OwnerUID: owner, Fiddles: 1, Bytes: int64(len(base.Snapshot.JSON))}
		for i := 0; i < 9; i++ {
			u.Revisions = append(u.Revisions, base.At.Add(59*time.Second))
		}
		seedUsage(owner, u)
		a := write(owner, "task3-rate-first-abcdefgh", "rate-one")
		b := write(owner, "task3-rate-second-abcdefg", "rate-two")
		a.At, b.At = base.At.Add(time.Minute), base.At.Add(time.Minute)
		requireRaceLimit(raceCreates(a, b), LimitRate)
		after := loadUsage(owner)
		if len(after.Revisions) != 10 || after.Fiddles != 2 {
			t.Fatal("rate race exceeded ten writes")
		}
	})
	t.Run("fiddle-race", func(t *testing.T) {
		owner := "task3-slots-owner"
		seedUsage(owner, usage{OwnerUID: owner, Fiddles: 99, Bytes: 99 * int64(len(base.Snapshot.JSON))})
		requireRaceLimit(raceCreates(write(owner, "task3-slot-first-abcdefgh", "one"), write(owner, "task3-slot-second-abcdefg", "two")), LimitFiddles)
		if loadUsage(owner).Fiddles != 100 {
			t.Fatal("fiddle cap wasn't atomic")
		}
	})
	t.Run("storage-race", func(t *testing.T) {
		owner := "task3-bytes-owner"
		seedUsage(owner, usage{OwnerUID: owner, Fiddles: 2, Bytes: MaxRetainedBytes - int64(len(base.Snapshot.JSON))})
		requireRaceLimit(raceCreates(write(owner, "task3-byte-first-abcdefgh", "one"), write(owner, "task3-byte-second-abcdefg", "two")), LimitStorage)
		if loadUsage(owner).Bytes != MaxRetainedBytes {
			t.Fatal("storage cap wasn't exact")
		}
	})
	t.Run("revision100-race", func(t *testing.T) {
		w := write("task3-revision-owner", "task3-revision-abcdefghij", "create")
		for n := 1; n <= 99; n++ {
			prior := w
			prior.ExpectedRevision = uint64(n - 1)
			prior.IdempotencyKey = fmt.Sprintf("prior-%d", n)
			prior.At = base.At.Add(time.Duration(n) * time.Minute)
			var err error
			if n == 1 {
				_, err = s.Create(ctx, prior)
			} else {
				_, err = s.Append(ctx, prior)
			}
			if err != nil {
				t.Fatalf("real prior revision%d: %v", n, err)
			}
		}

		a, b := w, w
		a.ExpectedRevision, b.ExpectedRevision = 99, 99
		a.IdempotencyKey, b.IdempotencyKey = "one", "two"
		a.At, b.At = base.At.Add(100*time.Minute), base.At.Add(100*time.Minute)
		start := make(chan struct{})
		results := make(chan outcome, 2)
		for _, w := range []publishing.Write{a, b} {
			go func(w publishing.Write) { <-start; r, e := s.Append(ctx, w); results <- outcome{r, e} }(w)
		}
		close(start)
		wins, conflicts := 0, 0
		for i := 0; i < 2; i++ {
			r := <-results
			if r.err == nil {
				wins++
			} else if errors.Is(r.err, publishing.ErrConflict) {
				conflicts++
			} else {
				t.Fatal(r.err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatal("revision99 append race failed")
		}
		a.ExpectedRevision = 100
		a.IdempotencyKey = "101"
		_, err := s.Append(ctx, a)
		requireLimit(t, err, LimitRevisions, "fork_fiddle")
		result, err := s.Read(ctx, w.ID, 0)
		if err != nil || result.Revision != 100 || result.Snapshot != w.Snapshot {
			t.Fatalf("latest read %+v %v", result, err)
		}
		var cleanupErr error
		for attempt := 0; attempt < 3; attempt++ {
			cleanupErr = s.Delete(ctx, publishing.Delete{OwnerUID: w.OwnerUID, ID: w.ID, At: a.At})
			if cleanupErr == nil {
				break
			}
			if !errors.Is(cleanupErr, publishing.ErrUnavailable) {
				t.Fatal(cleanupErr)
			}
		}
		if cleanupErr != nil {
			t.Fatal("100-revision cleanup did not resume", cleanupErr)
		}
		if u := loadUsage(w.OwnerUID); u.Bytes != 0 || u.Fiddles != 0 {
			t.Fatal("100-revision cleanup didn't release quota")
		}

	})
	t.Run("append-delete-and-competing-deleters", func(t *testing.T) {
		w := write("task3-delete-race-owner", "task3-delete-race-abcdefgh", "create")
		if _, err := s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		d := publishing.Delete{OwnerUID: w.OwnerUID, ID: w.ID, At: w.At}
		a := w
		a.ExpectedRevision = 1
		a.IdempotencyKey = "append"
		start := make(chan struct{})
		appendResult := make(chan error, 1)
		deletes := make(chan error, 2)
		go func() { <-start; _, err := s.Append(ctx, a); appendResult <- err }()
		for i := 0; i < 2; i++ {
			go func() { <-start; deletes <- s.Delete(ctx, d) }()
		}
		close(start)
		aerr := <-appendResult
		if aerr != nil && !errors.Is(aerr, publishing.ErrNotFound) {
			t.Fatal(aerr)
		}
		for i := 0; i < 2; i++ {
			err := <-deletes
			if err != nil && !errors.Is(err, publishing.ErrUnavailable) {
				t.Fatal(err)
			}
		}
		if err := s.Delete(ctx, d); err != nil {
			t.Fatal(err)
		}
		u := loadUsage(w.OwnerUID)
		wantRate := 1
		if aerr == nil {
			wantRate = 2
		}
		if u.Bytes != 0 || u.Fiddles != 0 || len(u.Revisions) != wantRate {
			t.Fatalf("delete race double released/refunded %+v", u)
		}
		if _, err := s.Read(ctx, w.ID, 1); !errors.Is(err, publishing.ErrNotFound) {
			t.Fatal("deleted pinned read survived")
		}
		if _, err := s.Create(ctx, w); !errors.Is(err, publishing.ErrNotFound) {
			t.Fatal("deleted receipt replay recreated source")
		}
	})
	t.Run("20MiB-payload-cleanup-and-uncertain-finalization", func(t *testing.T) {
		w := write("task3-maximum-owner", "task3-maximum-abcdefghijk", "revision-1")
		w.Snapshot = padded(t, snapshot.MaxBytes)
		for n := 1; n <= 40; n++ {
			w.IdempotencyKey = fmt.Sprintf("revision-%d", n)
			w.At = base.At.Add(time.Duration(n) * time.Minute)
			w.ExpectedRevision = uint64(n - 1)
			var err error
			if n == 1 {
				_, err = s.Create(ctx, w)
			} else {
				_, err = s.Append(ctx, w)
			}
			if err != nil {
				t.Fatalf("max payload revision%d: %v", n, err)
			}
		}
		initial := loadUsage(w.OwnerUID)
		if initial.Bytes != MaxRetainedBytes || initial.Fiddles != 1 {
			t.Fatalf("maximum retained bytes %+v", initial)
		}
		d := publishing.Delete{OwnerUID: w.OwnerUID, ID: w.ID, At: w.At}
		fault := &interruptedDB{DB: raw, failBefore: 3}
		resumable, err := New(fault)
		if err != nil {
			t.Fatal(err)
		}
		if err := resumable.Delete(ctx, d); !errors.Is(err, publishing.ErrUnavailable) {
			t.Fatalf("crash after batch returned %v", err)
		}
		doc, err := client.Doc(fiddleKey(w.ID).String()).Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var state fiddle
		if err := doc.DataTo(&state); err != nil || state.CleanupNext != 2 || !state.Deleted || state.CleanupDone {
			t.Fatalf("bad persisted progress %+v %v", state, err)
		}
		if _, err := client.Doc(revisionKey(w.ID, 1).String()).Get(ctx); err == nil {
			t.Fatal("batch didn't physically remove its payload")
		}
		if u := loadUsage(w.OwnerUID); !reflect.DeepEqual(u, initial) {
			t.Fatal("pending cleanup freed retained quota")
		}
		if _, err := s.Read(ctx, w.ID, 40); !errors.Is(err, publishing.ErrNotFound) {
			t.Fatal("pending payload remained public")
		}
		newWrite := write(w.OwnerUID, "task3-pending-new-abcdefgh", "pending")
		newWrite.At = w.At
		_, err = s.Create(ctx, newWrite)
		requireLimit(t, err, LimitStorage, "delete_fiddle")
		// Missing already-cleaned data is safe even when cursor progress wasn't
		// observed by the caller. Rate and byte reservations stay frozen meanwhile.
		if _, err := client.Doc(revisionKey(w.ID, 2).String()).Delete(ctx); err != nil {
			t.Fatal(err)
		}
		fault.failBefore = 0
		fault.dropFinal = true
		for attempt := 0; attempt < 3; attempt++ {
			err = resumable.Delete(ctx, d)
			if fault.finalDropped {
				break
			}
			if !errors.Is(err, publishing.ErrUnavailable) {
				t.Fatalf("resume failed before finalization: %v", err)
			}
		}
		if !fault.finalDropped || !errors.Is(err, publishing.ErrUnavailable) {
			t.Fatalf("lost final acknowledgement wasn't exercised: %v", err)
		}

		if err := s.Delete(ctx, d); err != nil {
			t.Fatal(err)
		}
		final := loadUsage(w.OwnerUID)
		if final.Bytes != 0 || final.Fiddles != 0 || !reflect.DeepEqual(final.Revisions, initial.Revisions) {
			t.Fatal("final quota release wasn't exactly once")
		}
		records, err := client.Collection(fiddleKey(w.ID).String() + "/revisions").Documents(ctx).GetAll()
		if err != nil || len(records) != 0 {
			t.Fatalf("payloads still retained: %d %v", len(records), err)
		}
		before := loadUsage(w.OwnerUID)
		if err := s.Delete(ctx, d); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(loadUsage(w.OwnerUID), before) {
			t.Fatal("uncertain finalization retry refunded again")
		}
	})
}
