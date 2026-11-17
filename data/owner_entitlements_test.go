package data

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newOwnerRec(maxAge time.Duration) (*OwnerEntitlements, *time.Time) {
	now := t0
	r := NewOwnerEntitlements(fake.NewDB(), maxAge)
	r.now = func() time.Time { return now }
	return r, &now
}

func TestRecordedValueRoundTripsWithDottedKey(t *testing.T) {
	r, _ := newOwnerRec(time.Hour)
	if err := r.Record(context.Background(), "u1", OwnerEntitlement{Key: "initiative.saved_rosters", Type: "limit", Limit: 4}); err != nil {
		t.Fatal(err)
	}

	e, err := r.Get(context.Background(), "u1", "initiative.saved_rosters")

	if err != nil || e.Limit != 4 || !e.ReadAt.Equal(t0) {
		t.Fatalf("Get = %+v, %v; want limit 4 read at %v", e, err, t0)
	}
}

func TestGetMissingKeyOrUserIsNotFound(t *testing.T) {
	r, _ := newOwnerRec(time.Hour)
	_ = r.Record(context.Background(), "u1", OwnerEntitlement{Key: "a", Limit: 1})

	_, noKey := r.Get(context.Background(), "u1", "b")
	_, noUser := r.Get(context.Background(), "u2", "a")

	if !errors.Is(noKey, ErrNotFound) || !errors.Is(noUser, ErrNotFound) {
		t.Fatalf("missing key = %v, missing user = %v; want ErrNotFound", noKey, noUser)
	}
}

func TestFreshnessAtAndPastTheMaxAge(t *testing.T) {
	r, now := newOwnerRec(time.Hour)
	_ = r.Record(context.Background(), "u1", OwnerEntitlement{Key: "a", Limit: 1})
	e, _ := r.Get(context.Background(), "u1", "a")

	tests := []struct {
		name  string
		age   time.Duration
		fresh bool
	}{
		{"just recorded", 0, true},
		{"inside", 59 * time.Minute, true},
		{"exactly the max age", time.Hour, true},
		{"one nanosecond past", time.Hour + 1, false},
		{"long stale", 8 * 24 * time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			*now = t0.Add(tt.age)
			if got := r.Fresh(e); got != tt.fresh {
				t.Fatalf("Fresh at age %v = %v, want %v", tt.age, got, tt.fresh)
			}
		})
	}
}

func TestRecordStampsEachKeyWithItsOwnReadTime(t *testing.T) {
	r, now := newOwnerRec(time.Hour)
	_ = r.Record(context.Background(), "u1", OwnerEntitlement{Key: "a", Limit: 1})
	*now = t0.Add(30 * time.Minute)
	_ = r.Record(context.Background(), "u1", OwnerEntitlement{Key: "b", Limit: 2})

	a, _ := r.Get(context.Background(), "u1", "a")
	b, _ := r.Get(context.Background(), "u1", "b")

	if !a.ReadAt.Equal(t0) || !b.ReadAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("read times a=%v b=%v; recording b must not restamp a", a.ReadAt, b.ReadAt)
	}
}

func TestInterleavedRecordersCannotRevertEachOthersKeys(t *testing.T) {
	r, _ := newOwnerRec(time.Hour)
	ctx := context.Background()
	_ = r.Record(ctx, "u1", OwnerEntitlement{Key: "a", Limit: 1}, OwnerEntitlement{Key: "b", Limit: 1})

	// Recorder one read both keys earlier and now writes only a; recorder two writes only b.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = r.Record(ctx, "u1", OwnerEntitlement{Key: "a", Limit: 9}) }()
	go func() { defer wg.Done(); _ = r.Record(ctx, "u1", OwnerEntitlement{Key: "b", Limit: 7}) }()
	wg.Wait()

	a, _ := r.Get(ctx, "u1", "a")
	b, _ := r.Get(ctx, "u1", "b")
	if a.Limit != 9 || b.Limit != 7 {
		t.Fatalf("a=%d b=%d, want 9 and 7: a write reverted the other's key", a.Limit, b.Limit)
	}
}

func TestRecordRejectsABadUserID(t *testing.T) {
	r, _ := newOwnerRec(time.Hour)
	if err := r.Record(context.Background(), "", OwnerEntitlement{Key: "a"}); err == nil {
		t.Fatal("Record with an empty user ID succeeded")
	}
}
