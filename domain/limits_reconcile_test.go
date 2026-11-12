package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

// degradedChecker answers like the entitlements client during an outage with fail-open.
type degradedChecker struct{}

func (degradedChecker) Check(_ context.Context, key string, _ entitlements.Policy) (entitlements.Result, error) {
	return entitlements.Result{Key: key, Degraded: true}, nil
}

func limitedRosters(t *testing.T, n int) (*Rosters, *Limits, []string) {
	t.Helper()
	rs := NewRosters(fake.NewDB())
	ids := make([]string, n)
	for i := range ids {
		r, err := rs.Create(ctx, "u1", "u1", string(rune('A'+i)), false, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.ID
	}
	return rs, NewLimits(Kind{Name: "rosters", Key: KeySavedRosters, Repo: rs.Items()}), ids
}

func activeCount(t *testing.T, rs *Rosters) int {
	t.Helper()
	states, err := rs.Items().ItemStates(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range states {
		if !s.Inactive {
			n++
		}
	}
	return n
}

func TestReconcileDeactivatesBeyondTheLimitAndReactivatesOnUpgrade(t *testing.T) {
	rs, lim, _ := limitedRosters(t, 3)

	if err := lim.Reconcile(ctx, "u1", stubChecker{limit: 1}); err != nil {
		t.Fatal(err)
	}
	if got := activeCount(t, rs); got != 1 {
		t.Fatalf("active after downgrade = %d, want 1", got)
	}
	if err := lim.Reconcile(ctx, "u1", stubChecker{limit: 5}); err != nil {
		t.Fatal(err)
	}
	if got := activeCount(t, rs); got != 3 {
		t.Fatalf("active after upgrade = %d, want 3", got)
	}
}

func TestReconcileChangesNothingDuringAnOutage(t *testing.T) {
	rs, lim, _ := limitedRosters(t, 2)

	degraded := lim.Reconcile(ctx, "u1", degradedChecker{})
	failed := lim.Reconcile(ctx, "u1", stubChecker{err: errors.New("down")})

	if degraded != nil || failed == nil {
		t.Fatalf("degraded = %v, failed = %v; want nil and an error", degraded, failed)
	}
	if got := activeCount(t, rs); got != 2 {
		t.Fatalf("active after outage = %d, want 2 untouched", got)
	}
}

func TestChooseRecordsTheOwnersActiveSet(t *testing.T) {
	rs, lim, ids := limitedRosters(t, 3)

	active, err := lim.Choose(ctx, "u1", "rosters", []string{ids[2]}, stubChecker{limit: 1})

	if err != nil || len(active) != 1 || active[0] != ids[2] {
		t.Fatalf("active = %v, %v; want only the chosen newest", active, err)
	}
	if got := activeCount(t, rs); got != 1 {
		t.Fatalf("stored active = %d, want 1", got)
	}
}

func TestChooseRejectsBadSelections(t *testing.T) {
	_, lim, ids := limitedRosters(t, 2)
	tests := []struct {
		name string
		kind string
		ids  []string
		chk  Checker
		want error
	}{
		{"unknown kind", "nope", nil, stubChecker{limit: 1}, data.ErrNotFound},
		{"over the limit", "rosters", ids, stubChecker{limit: 1}, data.ErrInvalid},
		{"not held", "rosters", []string{"ffffffffffffffffffffffff"}, stubChecker{limit: 1}, data.ErrInvalid},
		{"duplicate", "rosters", []string{ids[0], ids[0]}, stubChecker{limit: 2}, data.ErrInvalid},
		{"entitlements down fails closed", "rosters", []string{ids[0]}, stubChecker{err: errors.New("down")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lim.Choose(ctx, "u1", tt.kind, tt.ids, tt.chk)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
