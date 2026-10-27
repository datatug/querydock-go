package revisions

import (
	"github.com/dal-go/record"
	"github.com/datatug/querydock-go/internal/publishing"
	"time"
)

const (
	MaxRevisionsPerWindow = 10
	RateWindow            = time.Minute
	MaxFiddles            = 100
	MaxRevisions          = 100
	MaxRetainedBytes      = int64(20 * 1024 * 1024)
	LimitRate             = "revision_rate"
	LimitFiddles          = "active_fiddles"
	LimitRevisions        = "fiddle_revisions"
	LimitStorage          = "retained_snapshot_bytes"
)

type usage struct {
	OwnerUID  string      `firestore:"ownerUid"`
	Fiddles   int64       `firestore:"fiddles"`
	Bytes     int64       `firestore:"bytes"`
	Revisions []time.Time `firestore:"revisions"`
}

func usageKey(owner string) *record.Key {
	return record.NewKeyWithParentAndID(extension, "usage", hashFields("querydock-owner/1", owner))
}
func validUsage(u usage, owner string) bool {
	if u.OwnerUID != owner || u.Fiddles < 0 || u.Fiddles > MaxFiddles || u.Bytes < 0 || u.Bytes > MaxRetainedBytes || len(u.Revisions) > MaxRevisionsPerWindow || (u.Fiddles == 0 && u.Bytes != 0) || (u.Fiddles > 0 && u.Bytes == 0) {
		return false
	}
	var previous time.Time
	for _, at := range u.Revisions {
		if at.IsZero() || at.Before(previous) {
			return false
		}
		previous = at
	}
	return true
}
func charge(u *usage, state fiddle, w publishing.Write, appendRevision bool) error {
	if !validUsage(*u, w.OwnerUID) {
		return publishing.ErrUnavailable
	}
	if appendRevision && (u.Fiddles < 1 || state.Bytes < 1 || state.Bytes > u.Bytes) {
		return publishing.ErrUnavailable
	}
	if !appendRevision && u.Fiddles == MaxFiddles {
		return &publishing.LimitError{Code: LimitFiddles, Action: "delete_fiddle"}
	}
	size := int64(len(w.Snapshot.JSON))
	if size > MaxRetainedBytes-u.Bytes {
		return &publishing.LimitError{Code: LimitStorage, Action: "delete_fiddle"}
	}
	// Requests can enter transactions out of order or during a clock rollback.
	// Never move quota time backwards or expire a future accepted timestamp.
	now := w.At
	if n := len(u.Revisions); n > 0 && u.Revisions[n-1].After(now) {
		now = u.Revisions[n-1]
	}
	recent := make([]time.Time, 0, MaxRevisionsPerWindow)
	for _, at := range u.Revisions {
		if at.After(now.Add(-RateWindow)) {
			recent = append(recent, at)
		}
	}
	if len(recent) == MaxRevisionsPerWindow {
		remaining := recent[0].Add(RateWindow).Sub(w.At)
		wait := remaining / time.Second
		if remaining%time.Second != 0 {
			wait++
		}
		return &publishing.LimitError{Code: LimitRate, Action: "wait", RetryAfterSeconds: uint64(wait)}
	}
	u.Revisions = append(recent, now)
	u.Bytes += size
	if !appendRevision {
		u.Fiddles++
	}
	return nil
}
