package revisions

import (
	"cloud.google.com/go/firestore"
	"context"
	"errors"
	"github.com/dal-go/dalgo2firestore"
	"github.com/datatug/querydock-go/internal/publishing"
	"google.golang.org/api/option"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFirestoreEmulator(t *testing.T) {
	project := os.Getenv("QUERYDOCK_TEST_PROJECT")
	if project == "" {
		t.Skip("run scripts/sdk-emulators.py")
	}
	host, _, err := net.SplitHostPort(os.Getenv("FIRESTORE_EMULATOR_HOST"))
	if err != nil || host != "127.0.0.1" || !strings.HasPrefix(project, "demo-querydock-") {
		t.Fatal("refusing unowned/non-demo storage target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := firestore.NewClientWithDatabase(ctx, project, "(default)", option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Firestore SDK: %v", err)
		}
	}()
	s, err := New(dalgo2firestore.NewDatabase(project, client))
	if err != nil {
		t.Fatal(err)
	}
	_, _, w := setup(t)
	w.ID = "emulatorfirstabcdefghijkl"
	first, err := s.Create(ctx, w)
	if err != nil || first.Revision != 1 {
		t.Fatalf("real create %+v %v", first, err)
	}
	historyRef := client.Doc("ext/querydock/fiddles/" + w.ID + "/revisions/1")
	original, err := historyRef.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var history revision
	if err := original.DataTo(&history); err != nil || history.JSON != w.Snapshot.JSON || history.Digest != w.Snapshot.Digest {
		t.Fatalf("complete source changed %+v %v", history, err)
	}
	type outcome struct {
		receipt publishing.Receipt
		err     error
		write   publishing.Write
	}
	race := func(writes []publishing.Write, create bool) []outcome {
		t.Helper()
		var start sync.WaitGroup
		start.Add(1)
		results := make(chan outcome, len(writes))
		for _, write := range writes {
			go func(write publishing.Write) {
				start.Wait()
				var receipt publishing.Receipt
				var err error
				if create {
					receipt, err = s.Create(ctx, write)
				} else {
					receipt, err = s.Append(ctx, write)
				}
				results <- outcome{receipt, err, write}
			}(write)
		}
		start.Done()
		out := make([]outcome, len(writes))
		for i := range out {
			out[i] = <-results
		}
		return out
	}
	a, b := w, w
	a.ExpectedRevision, b.ExpectedRevision = 1, 1
	a.IdempotencyKey, b.IdempotencyKey = "race-a", "race-b"
	a.Snapshot = changed(t, w.Snapshot)
	results := race([]publishing.Write{a, b}, false)
	won, conflicted := 0, 0
	var winner publishing.Write
	var winnerReceipt publishing.Receipt
	for _, r := range results {
		if r.err == nil {
			won++
			winner, winnerReceipt = r.write, r.receipt
		} else if errors.Is(r.err, publishing.ErrConflict) {
			conflicted++
		} else {
			t.Fatal(r.err)
		}
	}
	if won != 1 || conflicted != 1 || winnerReceipt.Revision != 2 {
		t.Fatalf("real race %+v", results)
	}
	retry, err := s.Append(ctx, winner)
	if err != nil || retry != winnerReceipt {
		t.Fatalf("winner retry %+v %v", retry, err)
	}
	after, err := historyRef.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var preserved revision
	if err := after.DataTo(&preserved); err != nil || preserved != history {
		t.Fatal("race changed immutable history")
	}
	wrong := winner
	wrong.OwnerUID = "other-owner"
	wrong.IdempotencyKey = "other-key"
	wrong.ExpectedRevision = 2
	if _, err := s.Append(ctx, wrong); !errors.Is(err, publishing.ErrForbidden) {
		t.Fatalf("cross-owner append: %v", err)
	}
	changedBody := winner
	changedBody.Snapshot = changed(t, w.Snapshot)
	if changedBody.Snapshot == winner.Snapshot {
		changedBody.Snapshot = w.Snapshot
	}
	if _, err := s.Append(ctx, changedBody); !errors.Is(err, publishing.ErrConflict) {
		t.Fatalf("changed-body retry: %v", err)
	}
	c, d := w, w
	c.ID = "emulatorsecondabcdefghijk"
	d.ID = "emulatorthirdabcdefghijkl"
	c.IdempotencyKey, d.IdempotencyKey = "same-create-key", "same-create-key"
	same := race([]publishing.Write{c, d}, true)
	if same[0].err != nil || same[1].err != nil || same[0].receipt != same[1].receipt {
		t.Fatalf("concurrent idempotency %+v", same)
	}
	unused := c.ID
	if unused == same[0].receipt.ID {
		unused = d.ID
	}
	if _, err := client.Doc("ext/querydock/fiddles/" + unused).Get(ctx); err == nil {
		t.Fatal("retry created second candidate")
	}
	// Changed bodies racing with one owner/key must not silently select two
	// revisions or overwrite the winner's immutable bytes.
	e, f := w, w
	e.ID, f.ID = "emulatorbodyoneabcdefghij", "emulatorbodytwoabcdefghij"
	e.IdempotencyKey, f.IdempotencyKey = "body-race", "body-race"
	f.Snapshot = changed(t, w.Snapshot)
	bodyRace := race([]publishing.Write{e, f}, true)
	bodyWins, bodyConflicts := 0, 0
	for _, result := range bodyRace {
		if result.err == nil {
			bodyWins++
			doc, err := client.Doc("ext/querydock/fiddles/" + result.receipt.ID + "/revisions/1").Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var stored revision
			if err := doc.DataTo(&stored); err != nil || stored.JSON != result.write.Snapshot.JSON {
				t.Fatal("body race replaced winning snapshot")
			}
		} else if errors.Is(result.err, publishing.ErrConflict) {
			bodyConflicts++
		} else {
			t.Fatal(result.err)
		}
	}
	if bodyWins != 1 || bodyConflicts != 1 {
		t.Fatalf("body race %+v", bodyRace)
	}
	// Two different owners colliding on one random candidate ID cannot overwrite
	// one another, even when both transactions initially observe a missing key.
	g, h := w, w
	g.ID, h.ID = "emulatorcollisionabcdefgh", "emulatorcollisionabcdefgh"
	g.IdempotencyKey, h.IdempotencyKey = "collision-race-one", "collision-race-two"
	h.OwnerUID = "second-verified-owner"
	collisionRace := race([]publishing.Write{g, h}, true)
	collisionWins, collisionConflicts := 0, 0
	for _, result := range collisionRace {
		if result.err == nil {
			collisionWins++
			doc, err := client.Doc("ext/querydock/fiddles/" + result.receipt.ID).Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var stored fiddle
			if err := doc.DataTo(&stored); err != nil || stored.OwnerUID != result.write.OwnerUID {
				t.Fatal("collision overwrote owner")
			}
		} else if errors.Is(result.err, publishing.ErrConflict) {
			collisionConflicts++
		} else {
			t.Fatal(result.err)
		}
	}
	if collisionWins != 1 || collisionConflicts != 1 {
		t.Fatalf("ID collision race %+v", collisionRace)
	}
	collision := w
	collision.OwnerUID = "another-owner"
	collision.IdempotencyKey = "collision"
	if _, err := s.Create(ctx, collision); !errors.Is(err, publishing.ErrConflict) {
		t.Fatalf("ID collision: %v", err)
	}
	// Tombstone creation is Task 3. Existing tombstones already fence retries.
	if _, err := client.Doc("ext/querydock/fiddles/"+w.ID).Update(ctx, []firestore.Update{{Path: "deleted", Value: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, w); !errors.Is(err, publishing.ErrNotFound) {
		t.Fatalf("deleted replay: %v", err)
	}
	if _, err := s.Append(ctx, winner); !errors.Is(err, publishing.ErrNotFound) {
		t.Fatalf("deleted append replay: %v", err)
	}
}
