package revisions

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/snapshot"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const firstID = "abcdefghijklmnopqrstuv"
const otherID = "zyxwvutsrqponmlkjihgfeA"

// This fake exists only in tests. Each callback gets a detached journal; failed
// callbacks/commits cannot leak writes. Real Firestore races are tested separately.
type fakeDB struct {
	dal.DB
	mu                       sync.Mutex
	data                     map[string][]byte
	runs, attempts           int
	readRuns                 int
	failRunAt, afterCommitAt int
	afterCommitErr           error
	dropCommit               bool
	faultOp                  string
	faultAt                  int
	runErr, commitErr        error
	replay                   bool
}
type fakeTx struct {
	dal.ReadwriteTransaction
	data   map[string][]byte
	db     *fakeDB
	counts map[string]int
}

func cloneData(data map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range data {
		out[k] = append([]byte(nil), v...)
	}
	return out
}
func (d *fakeDB) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, _ ...dal.TransactionOption) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs++
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.runErr != nil && (d.failRunAt == 0 || d.runs == d.failRunAt) {
		return d.runErr
	}
	attempts := 1
	if d.replay {
		attempts = 2
	}
	for i := 0; i < attempts; i++ {
		d.attempts++
		tx := &fakeTx{data: cloneData(d.data), db: d, counts: map[string]int{}}
		if err := f(ctx, tx); err != nil {
			return err
		}
		if i == attempts-1 {
			if d.commitErr != nil {
				return d.commitErr
			}
			if !d.dropCommit {
				d.data = tx.data
			}
			if d.afterCommitErr != nil && d.runs == d.afterCommitAt {
				return d.afterCommitErr
			}
		}
	}
	return nil
}

func (d *fakeDB) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, _ ...dal.TransactionOption) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readRuns++
	if d.runErr != nil {
		return d.runErr
	}
	return f(ctx, &fakeTx{data: cloneData(d.data), db: d, counts: map[string]int{}})
}
func (tx *fakeTx) Delete(_ context.Context, key *record.Key) error {
	if err := tx.fault("delete"); err != nil {
		return err
	}
	delete(tx.data, key.String())
	return nil
}

var errInjected = errors.New("injected transaction failure")

func (tx *fakeTx) fault(op string) error {
	tx.counts[op]++
	if tx.db.faultOp == op && tx.db.faultAt == tx.counts[op] {
		return errInjected
	}
	return nil
}
func (tx *fakeTx) Get(_ context.Context, r record.Record) error {
	if err := tx.fault("get"); err != nil {
		return err
	}
	raw, exists := tx.data[r.Key().String()]
	if !exists {
		r.SetError(record.ErrRecordNotFound)
		return record.ErrRecordNotFound
	}
	if err := json.Unmarshal(raw, r.Data()); err != nil {
		return err
	}
	r.SetError(nil)
	return nil
}
func (tx *fakeTx) Insert(_ context.Context, r record.Record, _ ...dal.InsertOption) error {
	if err := tx.fault("insert"); err != nil {
		return err
	}
	key := r.Key().String()
	if _, exists := tx.data[key]; exists {
		return record.ErrRecordExists
	}
	raw, err := json.Marshal(r.Data())
	if err == nil {
		tx.data[key] = raw
	}
	return err
}
func (tx *fakeTx) Set(_ context.Context, r record.Record) error {
	if err := tx.fault("set"); err != nil {
		return err
	}
	raw, err := json.Marshal(r.Data())
	if err == nil {
		tx.data[r.Key().String()] = raw
	}
	return err
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
func changed(t *testing.T, s snapshot.Validated) snapshot.Validated {
	t.Helper()
	result, err := snapshot.Import([]byte(strings.Replace(s.JSON, "Harbour calls", "Edited harbour", 1)))
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func setup(t *testing.T) (*Store, *fakeDB, publishing.Write) {
	t.Helper()
	db := &fakeDB{data: map[string][]byte{}}
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db, publishing.Write{OwnerUID: "owner/with%unsafe#path", ID: firstID, IdempotencyKey: "request-one", Snapshot: fixture(t), At: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
}
func load[T any](t *testing.T, db *fakeDB, key *record.Key) T {
	t.Helper()
	var data T
	if err := json.Unmarshal(db.data[key.String()], &data); err != nil {
		t.Fatal(err)
	}
	return data
}
func put(t *testing.T, db *fakeDB, key *record.Key, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	db.data[key.String()] = raw
}
func TestCreateAppendAndIdempotentReceipts(t *testing.T) {
	s, db, w := setup(t)
	receipt, err := s.Create(context.Background(), w)
	if err != nil || receipt.ID != firstID || receipt.Revision != 1 || receipt.Digest != w.Snapshot.Digest || receipt.CanonicalURL != "https://querydock.run/sql/"+firstID+"/1" {
		t.Fatalf("create %+v %v", receipt, err)
	}
	if len(db.data) != 4 {
		t.Fatal("create wasn't one three-record publication")
	}
	original := string(db.data[revisionKey(firstID, 1).String()])
	history := load[revision](t, db, revisionKey(firstID, 1))
	if history.JSON != w.Snapshot.JSON || history.Digest != w.Snapshot.Digest || history.Number != 1 || history.CreatedAt != w.At {
		t.Fatal("snapshot/history changed")
	}
	state := load[fiddle](t, db, fiddleKey(firstID))
	if state.OwnerUID != w.OwnerUID || state.Latest != 1 {
		t.Fatal("owner/latest missing")
	}
	if strings.Contains(ledgerKey(w.OwnerUID, w.IdempotencyKey).String(), w.OwnerUID) {
		t.Fatal("unsafe UID interpolated into path")
	}
	retry := w
	retry.ID = otherID
	retry.At = retry.At.Add(time.Hour)
	again, err := s.Create(context.Background(), retry)
	if err != nil || again != receipt || len(db.data) != 4 {
		t.Fatalf("create retry changed identity/history %+v %v", again, err)
	}
	appendWrite := w
	appendWrite.ExpectedRevision = 1
	appendWrite.IdempotencyKey = "append-one"
	appendWrite.Snapshot = changed(t, w.Snapshot)
	appendWrite.At = w.At.Add(time.Minute)
	second, err := s.Append(context.Background(), appendWrite)
	if err != nil || second.Revision != 2 || second.Digest != appendWrite.Snapshot.Digest {
		t.Fatalf("append %+v %v", second, err)
	}
	if len(db.data) != 6 || string(db.data[revisionKey(firstID, 1).String()]) != original {
		t.Fatal("append overwrote prior revision")
	}
	state = load[fiddle](t, db, fiddleKey(firstID))
	if state.CreatedAt != w.At || state.UpdatedAt != appendWrite.At || state.Latest != 2 {
		t.Fatal("append lost lineage")
	}
	third := appendWrite
	third.IdempotencyKey = "append-two"
	third.ExpectedRevision = 2
	if _, err := s.Append(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	again, err = s.Append(context.Background(), appendWrite)
	if err != nil || again != second || len(db.data) != 8 {
		t.Fatalf("retry after a newer revision failed %+v %v", again, err)
	}
}
func TestRejectInvalidWritesWithoutTransaction(t *testing.T) {
	for _, kind := range []string{"owner", "id", "key", "create-revision", "append-zero", "overflow", "snapshot", "digest", "noncanonical"} {
		t.Run(kind, func(t *testing.T) {
			s, db, w := setup(t)
			appendRevision := false
			switch kind {
			case "owner":
				w.OwnerUID = ""
			case "id":
				w.ID = "reserved"
			case "key":
				w.IdempotencyKey = "bad key"
			case "create-revision":
				w.ExpectedRevision = 1
			case "append-zero":
				appendRevision = true
			case "overflow":
				appendRevision = true
				w.ExpectedRevision = math.MaxInt64
			case "snapshot":
				w.Snapshot.JSON = `{}`
			case "digest":
				w.Snapshot.Digest = "forged"
			case "noncanonical":
				w.Snapshot.JSON = " " + w.Snapshot.JSON
			}
			var err error
			if appendRevision {
				_, err = s.Append(context.Background(), w)
			} else {
				_, err = s.Create(context.Background(), w)
			}
			if !errors.Is(err, publishing.ErrConflict) || db.runs != 0 || len(db.data) != 0 {
				t.Fatal("invalid write reached transaction")
			}
		})
	}
	if _, err := New(nil); err == nil {
		t.Fatal("nil DB accepted")
	}
}
func TestOwnershipConflictsAndTombstoneFence(t *testing.T) {
	for _, kind := range []string{"missing", "other-owner", "deleted", "stale", "invalid-latest", "candidate-collision", "orphan-revision", "changed-create-key", "changed-append-key", "cross-operation-key", "changed-target", "changed-expected"} {
		t.Run(kind, func(t *testing.T) {
			s, db, w := setup(t)
			if kind != "missing" && kind != "orphan-revision" {
				if _, err := s.Create(context.Background(), w); err != nil {
					t.Fatal(err)
				}
			}
			attempt := w
			attempt.IdempotencyKey = "append"
			attempt.ExpectedRevision = 1
			want := publishing.ErrConflict
			create := false
			switch kind {
			case "missing":
				want = publishing.ErrNotFound
			case "other-owner":
				attempt.OwnerUID = "another-owner"
				want = publishing.ErrForbidden
			case "deleted":
				state := load[fiddle](t, db, fiddleKey(w.ID))
				state.Deleted = true
				put(t, db, fiddleKey(w.ID), state)
				want = publishing.ErrNotFound
			case "stale":
				attempt.ExpectedRevision = 2
			case "invalid-latest":
				state := load[fiddle](t, db, fiddleKey(w.ID))
				state.Latest = 0
				put(t, db, fiddleKey(w.ID), state)
			case "candidate-collision":
				create = true
				attempt.ExpectedRevision = 0
			case "orphan-revision":
				create = true
				attempt.ExpectedRevision = 0
				put(t, db, revisionKey(w.ID, 1), revision{Number: 1})
			case "changed-create-key":
				create = true
				attempt = w
				attempt.Snapshot = changed(t, w.Snapshot)
			case "cross-operation-key":
				attempt.IdempotencyKey = w.IdempotencyKey
			case "changed-append-key", "changed-target", "changed-expected":
				if _, err := s.Append(context.Background(), attempt); err != nil {
					t.Fatal(err)
				}
				if kind == "changed-append-key" {
					attempt.Snapshot = changed(t, w.Snapshot)
				}
				if kind == "changed-target" {
					attempt.ID = otherID
				}
				if kind == "changed-expected" {
					attempt.ExpectedRevision = 2
				}
			}
			before := cloneData(db.data)
			var err error
			if create {
				_, err = s.Create(context.Background(), attempt)
			} else {
				_, err = s.Append(context.Background(), attempt)
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: got %v want %v", kind, err, want)
			}
			if len(db.data) != len(before) {
				t.Fatal("failed write created records")
			}
			for key, raw := range before {
				if string(db.data[key]) != string(raw) {
					t.Fatal("failed write changed existing record")
				}
			}
		})
	}
}
func TestReplayIntegrityAndRevocation(t *testing.T) {
	for _, kind := range []string{"ledger-owner", "fingerprint", "id", "revision", "digest", "missing", "deleted", "owner", "latest", "get-error", "append-id", "append-revision"} {
		t.Run(kind, func(t *testing.T) {
			s, db, w := setup(t)
			if _, err := s.Create(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			appendRevision := strings.HasPrefix(kind, "append-")
			if appendRevision {
				w.ExpectedRevision = 1
				w.IdempotencyKey = "append"
				if _, err := s.Append(context.Background(), w); err != nil {
					t.Fatal(err)
				}
			}
			saved := load[ledger](t, db, ledgerKey(w.OwnerUID, w.IdempotencyKey))
			state := load[fiddle](t, db, fiddleKey(w.ID))
			want := publishing.ErrUnavailable
			switch kind {
			case "ledger-owner":
				saved.OwnerUID = "other"
				want = publishing.ErrConflict
			case "fingerprint":
				saved.Fingerprint = "changed"
				want = publishing.ErrConflict
			case "id", "append-id":
				saved.ID = "invalid"
			case "revision":
				saved.Revision = 0
			case "append-revision":
				saved.Revision = 3
			case "digest":
				saved.Digest = "changed"
			case "missing":
				delete(db.data, fiddleKey(w.ID).String())
				want = publishing.ErrNotFound
			case "deleted":
				state.Deleted = true
				want = publishing.ErrNotFound
			case "owner":
				state.OwnerUID = "other"
				want = publishing.ErrForbidden
			case "latest":
				state.Latest = 0
			case "get-error":
				db.faultOp, db.faultAt = "get", 2
				want = errInjected
			}
			put(t, db, ledgerKey(w.OwnerUID, w.IdempotencyKey), saved)
			if kind != "missing" {
				put(t, db, fiddleKey(w.ID), state)
			}
			var err error
			if appendRevision {
				_, err = s.Append(context.Background(), w)
			} else {
				_, err = s.Create(context.Background(), w)
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
		})
	}
}
func TestTransactionFailuresAndRetries(t *testing.T) {
	for _, op := range []string{"get", "insert", "append-insert", "set", "run", "commit", "commit-exists"} {
		max := 1
		if op == "get" || op == "insert" || op == "append-insert" {
			max = 3
		}
		for at := 1; at <= max; at++ {
			s, db, w := setup(t)
			if op == "set" || op == "append-insert" {
				if _, err := s.Create(context.Background(), w); err != nil {
					t.Fatal(err)
				}
				w.ExpectedRevision = 1
				w.IdempotencyKey = "append"
			}
			db.faultOp, db.faultAt = op, at
			if op == "append-insert" {
				db.faultOp = "insert"
				if at == 3 {
					continue
				}
			}
			want := errInjected
			if op == "run" {
				db.runErr = errInjected
			}
			if op == "commit" {
				db.commitErr = errInjected
			}
			if op == "commit-exists" {
				db.commitErr = record.ErrRecordExists
				want = publishing.ErrConflict
			}
			before := cloneData(db.data)
			var receipt publishing.Receipt
			var err error
			if op == "set" || op == "append-insert" {
				receipt, err = s.Append(context.Background(), w)
			} else {
				receipt, err = s.Create(context.Background(), w)
			}
			if !errors.Is(err, want) || receipt != (publishing.Receipt{}) {
				t.Fatalf("%s %d: receipt %+v err %v", op, at, receipt, err)
			}
			if len(before) != len(db.data) {
				t.Fatal("failed transaction partially committed")
			}
			for key, raw := range before {
				if string(db.data[key]) != string(raw) {
					t.Fatal("failed transaction changed committed bytes")
				}
			}
		}
	}
	s, db, w := setup(t)
	db.replay = true
	receipt, err := s.Create(context.Background(), w)
	if err != nil || db.attempts != 2 || len(db.data) != 4 || receipt.ID != w.ID {
		t.Fatalf("callback replay %+v %v attempts%d", receipt, err, db.attempts)
	}
	if result, err := s.Read(context.Background(), w.ID, 1); err != nil || result.Snapshot != w.Snapshot {
		t.Fatalf("public snapshot %+v %v", result, err)
	}
	if err := s.Delete(context.Background(), publishing.Delete{OwnerUID: w.OwnerUID, ID: w.ID, At: w.At}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), w.ID, 1); !errors.Is(err, publishing.ErrNotFound) {
		t.Fatal("deleted read remained available")
	}

}
func TestConcurrentUnitWrites(t *testing.T) {
	s, db, w := setup(t)
	if _, err := s.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		go func(key string) {
			<-start
			copy := w
			copy.IdempotencyKey = key
			copy.ExpectedRevision = 1
			_, err := s.Append(context.Background(), copy)
			results <- err
		}(key)
	}
	close(start)
	ok, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			ok++
		} else if errors.Is(err, publishing.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 || load[fiddle](t, db, fiddleKey(w.ID)).Latest != 2 || len(db.data) != 6 {
		t.Fatal("race overwrote history")
	}
}
func TestFramingAndRootPolicy(t *testing.T) {
	if hashFields("ab", "c") == hashFields("a", "bc") {
		t.Fatal("ambiguous fingerprint framing")
	}
	s, db, _ := setup(t)
	err := s.db.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Insert(ctx, record.NewRecordWithData(record.NewKeyWithParentAndID(record.NewKeyWithID("ext", "unregistered-querydock"), "fiddles", firstID), &fiddle{}))
	})
	if err == nil || len(db.data) != 0 {
		t.Fatal("mandatory Core root guard was bypassed")
	}
}
