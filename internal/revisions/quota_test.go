package revisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/datatug/querydock-go/internal/publishing"
	"github.com/datatug/querydock-go/internal/snapshot"
	"strings"
	"testing"
	"time"
)

func requireLimit(t *testing.T, err error, code, action string) uint64 {
	t.Helper()
	var limit *publishing.LimitError
	if !errors.As(err, &limit) || limit.Code != code || limit.Action != action {
		t.Fatalf("got %v want limit %s/%s", err, code, action)
	}
	return limit.RetryAfterSeconds
}
func saveAppend(t *testing.T, s *Store, w publishing.Write, n int, at time.Time) publishing.Write {
	t.Helper()
	w.ExpectedRevision = uint64(n - 1)
	w.IdempotencyKey = fmt.Sprintf("revision-%d", n)
	w.At = at
	var err error
	if n == 1 {
		_, err = s.Create(context.Background(), w)
	} else {
		_, err = s.Append(context.Background(), w)
	}
	if err != nil {
		t.Fatalf("save revision %d: %v", n, err)
	}
	return w
}
func padded(t *testing.T, size int) snapshot.Validated {
	t.Helper()
	base := fixture(t)
	var value map[string]any
	if err := json.Unmarshal([]byte(base.JSON), &value); err != nil {
		t.Fatal(err)
	}
	value["schemaSql"] = ""
	raw, _ := json.Marshal(value)
	canonical, err := snapshot.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	value["schemaSql"] = strings.Repeat("x", size-len(canonical))
	raw, _ = json.Marshal(value)
	canonical, err = snapshot.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := snapshot.Import([]byte(canonical))
	if err != nil || len(result.JSON) != size {
		t.Fatalf("padded fixture %d: %v", len(result.JSON), err)
	}
	return result
}
func TestRollingRateAndUnchargedRetries(t *testing.T) {
	s, db, w := setup(t)
	at := w.At.Add(59 * time.Second)
	var last publishing.Write
	for n := 1; n <= 10; n++ {
		last = saveAppend(t, s, w, n, at)
	}
	before := string(db.data[usageKey(w.OwnerUID).String()])
	if _, err := s.Append(context.Background(), last); err != nil {
		t.Fatal(err)
	}
	retry := w
	retry.ID = otherID
	retry.IdempotencyKey = "revision-1"
	if _, err := s.Create(context.Background(), retry); err != nil {
		t.Fatal(err)
	}
	if string(db.data[usageKey(w.OwnerUID).String()]) != before {
		t.Fatal("live retry consumed quota")
	}
	for _, tc := range []struct {
		offset time.Duration
		retry  uint64
	}{{time.Second, 59}, {time.Millisecond * 59500, 1}, {-5 * time.Second, 65}} {
		attempt := w
		attempt.ExpectedRevision = 10
		attempt.IdempotencyKey = "blocked"
		attempt.At = at.Add(tc.offset)
		_, err := s.Append(context.Background(), attempt)
		retry := requireLimit(t, err, LimitRate, "wait")
		if retry != tc.retry {
			t.Fatalf("retry %d want %d", retry, tc.retry)
		}
		if string(db.data[usageKey(w.OwnerUID).String()]) != before {
			t.Fatal("rejected quota changed usage")
		}
	}
	saveAppend(t, s, w, 11, at.Add(time.Minute))
	u := load[usage](t, db, usageKey(w.OwnerUID))
	if len(u.Revisions) != 1 || u.Bytes != 11*int64(len(w.Snapshot.JSON)) || u.Fiddles != 1 {
		t.Fatalf("wrong rolling expiry %+v", u)
	}
}
func TestOutOfOrderClockIsConservative(t *testing.T) {
	s, db, w := setup(t)
	saveAppend(t, s, w, 1, w.At.Add(10*time.Second))
	saveAppend(t, s, w, 2, w.At.Add(5*time.Second))
	u := load[usage](t, db, usageKey(w.OwnerUID))
	if len(u.Revisions) != 2 || !u.Revisions[0].Equal(u.Revisions[1]) {
		t.Fatal("late request moved quota time backwards")
	}
}
func TestExactQuotaBoundaries(t *testing.T) {
	t.Run("fiddles", func(t *testing.T) {
		s, db, w := setup(t)
		size := int64(len(w.Snapshot.JSON))
		put(t, db, usageKey(w.OwnerUID), usage{OwnerUID: w.OwnerUID, Fiddles: 99, Bytes: 99 * size})
		if _, err := s.Create(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		w.ID = otherID
		w.IdempotencyKey = "one-too-many"
		_, err := s.Create(context.Background(), w)
		requireLimit(t, err, LimitFiddles, "delete_fiddle")
		u := load[usage](t, db, usageKey(w.OwnerUID))
		if u.Fiddles != 100 || u.Bytes != 100*size {
			t.Fatal("fiddle quota partially charged")
		}
	})
	t.Run("bytes", func(t *testing.T) {
		s, db, w := setup(t)
		saveAppend(t, s, w, 1, w.At)
		size := int64(len(w.Snapshot.JSON))
		put(t, db, usageKey(w.OwnerUID), usage{OwnerUID: w.OwnerUID, Fiddles: 2, Bytes: MaxRetainedBytes - size})
		saveAppend(t, s, w, 2, w.At)
		attempt := w
		attempt.ExpectedRevision = 2
		attempt.IdempotencyKey = "too-many-bytes"
		_, err := s.Append(context.Background(), attempt)
		requireLimit(t, err, LimitStorage, "delete_fiddle")
		if load[usage](t, db, usageKey(w.OwnerUID)).Bytes != MaxRetainedBytes {
			t.Fatal("wrong byte bound")
		}
	})
	t.Run("revisions", func(t *testing.T) {
		s, db, w := setup(t)
		var last publishing.Write
		for n := 1; n <= MaxRevisions; n++ {
			last = saveAppend(t, s, w, n, w.At.Add(time.Duration(n)*time.Minute))
		}
		attempt := last
		attempt.ExpectedRevision = 100
		attempt.IdempotencyKey = "revision-101"
		_, err := s.Append(context.Background(), attempt)
		requireLimit(t, err, LimitRevisions, "fork_fiddle")
		if _, err := s.Append(context.Background(), last); err != nil {
			t.Fatal("limit rejected a live receipt retry")
		}
		if load[fiddle](t, db, fiddleKey(w.ID)).Latest != 100 {
			t.Fatal("revision limit changed history")
		}
	})
}
func TestQuotaValidationAndCounterFailures(t *testing.T) {
	for _, kind := range []string{"owner", "negative-fiddles", "many-fiddles", "negative-bytes", "many-bytes", "unreserved-bytes", "empty-reservation", "many-times", "zero-time", "unordered", "append-unreserved", "append-bytes"} {
		t.Run(kind, func(t *testing.T) {
			s, db, w := setup(t)
			saveAppend(t, s, w, 1, w.At)
			u := load[usage](t, db, usageKey(w.OwnerUID))
			switch kind {
			case "owner":
				u.OwnerUID = "other"
			case "negative-fiddles":
				u.Fiddles = -1
			case "many-fiddles":
				u.Fiddles = 101
			case "negative-bytes":
				u.Bytes = -1
			case "many-bytes":
				u.Bytes = MaxRetainedBytes + 1
			case "unreserved-bytes":
				u.Fiddles = 0
			case "empty-reservation":
				u.Bytes = 0
			case "many-times":
				u.Revisions = make([]time.Time, 11)
			case "zero-time":
				u.Revisions = []time.Time{{}}
			case "unordered":
				u.Revisions = []time.Time{w.At, w.At.Add(-time.Second)}
			case "append-unreserved":
				u.Fiddles = 0
				u.Bytes = 0
			case "append-bytes":
				u.Bytes = 1
			}
			put(t, db, usageKey(w.OwnerUID), u)
			before := cloneData(db.data)
			w.ExpectedRevision = 1
			w.IdempotencyKey = "append"
			_, err := s.Append(context.Background(), w)
			if !errors.Is(err, publishing.ErrUnavailable) {
				t.Fatalf("corrupt usage allowed: %v", err)
			}
			for key, raw := range before {
				if string(db.data[key]) != string(raw) {
					t.Fatal("invalid counters changed persisted data")
				}
			}
		})
	}
	for _, op := range []string{"get", "set"} {
		s, db, w := setup(t)
		db.faultOp = op
		if op == "get" {
			db.faultAt = 4
		} else {
			db.faultAt = 1
		}
		if _, err := s.Create(context.Background(), w); !errors.Is(err, errInjected) {
			t.Fatalf("counter %s error %v", op, err)
		}
		if len(db.data) != 0 {
			t.Fatal("counter failure partially created snapshot")
		}
	}
	for _, kind := range []string{"time", "uid-long", "uid-utf8"} {
		s, db, w := setup(t)
		switch kind {
		case "time":
			w.At = time.Time{}
		case "uid-long":
			w.OwnerUID = strings.Repeat("a", 129)
		case "uid-utf8":
			w.OwnerUID = string([]byte{255})
		}
		if _, err := s.Create(context.Background(), w); !errors.Is(err, publishing.ErrConflict) || db.runs != 0 {
			t.Fatal("invalid authority/time reached quota store")
		}
	}
}
