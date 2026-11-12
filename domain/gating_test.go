package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// free is a caller without the game_systems_extra flag.
var free = WithChecker(context.Background(), stubChecker{flag: false})

func gatedService(t *testing.T) *Encounters {
	t.Helper()
	s := newEncService(t)
	zero := 0
	none := data.GameSystem{ID: data.DefaultSystemID, Name: "No System", Family: "simple",
		Fields: []data.TrackerField{{Key: "health", Label: "Health", Kind: data.FieldCounter, Def: 1, Min: &zero}}}
	if _, err := s.systems.Upsert(ctx, none); err != nil {
		t.Fatal(err)
	}
	return s
}

func newIn(t *testing.T, s *Encounters, system string) data.Encounter {
	t.Helper()
	e, err := s.Create(ctx, "u1", "u1", data.Encounter{Name: "x", GameSystemID: system,
		Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func isRequired(err error) bool {
	var er *EntitlementRequiredError
	return errors.As(err, &er) && er.Key == KeyGameSystemsExtra
}

func TestFreeUserCannotCreateInAGatedSystemButCanInNoSystem(t *testing.T) {
	s := gatedService(t)
	enc := data.Encounter{Name: "x", GameSystemID: "test", Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom}
	if _, err := s.Create(free, "u1", "u1", enc); !isRequired(err) {
		t.Fatalf("gated create by a free user = %v, want the entitlement error", err)
	}
	enc.GameSystemID = data.DefaultSystemID
	if _, err := s.Create(free, "u1", "u1", enc); err != nil {
		t.Fatalf("No System create by a free user = %v", err)
	}
}

func TestGatedCreateFailsClosedWhenEntitlementsAreDownOrAbsent(t *testing.T) {
	s := gatedService(t)
	enc := data.Encounter{Name: "x", GameSystemID: "test", Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom}
	down := WithChecker(context.Background(), stubChecker{err: entitlements.ErrUnavailable})
	if _, err := s.Create(down, "u1", "u1", enc); !errors.Is(err, entitlements.ErrUnavailable) {
		t.Fatalf("outage = %v, want ErrUnavailable", err)
	}
	if _, err := s.Create(context.Background(), "u1", "u1", enc); !errors.Is(err, entitlements.ErrUnavailable) {
		t.Fatalf("no checker = %v, want a refusal", err)
	}
}

func TestFreeUserCannotSwitchToAGatedSystem(t *testing.T) {
	s := gatedService(t)
	e := newIn(t, s, data.DefaultSystemID)
	_, err := s.UpdateSettings(free, "u1", "u1", e.ID, e.Revision, map[string]any{"game_system_id": "test"})
	if !isRequired(err) {
		t.Fatalf("switch = %v, want the entitlement error", err)
	}
	if _, err := s.UpdateSettings(free, "u1", "u1", e.ID, e.Revision, map[string]any{"name": "renamed"}); err != nil {
		t.Fatalf("non-system edit = %v", err)
	}
}

func TestLosingTheFlagLocksGatedEncountersAndRestoringItUnlocksThemUnchanged(t *testing.T) {
	s := gatedService(t)
	gated, plain := newIn(t, s, "test"), newIn(t, s, data.DefaultSystemID)
	locks := s.Locks()
	if err := locks.Reconcile(ctx, "u1", stubChecker{flag: false}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "u1", gated.ID)
	if err != nil || got.LockedAt == nil || got.Name != gated.Name || got.Revision != gated.Revision {
		t.Fatalf("locked encounter = %+v, %v; want readable, locked, untouched", got, err)
	}
	if p, _ := s.Get(ctx, "u1", plain.ID); p.LockedAt != nil {
		t.Fatal("a No System encounter was locked")
	}
	if _, err := s.UpdateSettings(ctx, "u1", "u1", gated.ID, gated.Revision, map[string]any{"name": "x"}); !errors.Is(err, data.ErrLocked) {
		t.Fatalf("edit locked = %v, want ErrLocked", err)
	}
	if _, err := s.Start(ctx, "u1", "u1", gated.ID, gated.Revision); !errors.Is(err, data.ErrLocked) {
		t.Fatalf("run locked = %v, want ErrLocked", err)
	}
	if err := locks.Reconcile(ctx, "u1", stubChecker{flag: true}); err != nil {
		t.Fatal(err)
	}
	back, _ := s.Get(ctx, "u1", gated.ID)
	if back.LockedAt != nil || back.Revision != gated.Revision {
		t.Fatalf("restored encounter = %+v, want unlocked and unchanged", back)
	}
	if _, err := s.UpdateSettings(ctx, "u1", "u1", gated.ID, gated.Revision, map[string]any{"name": "ok"}); err != nil {
		t.Fatalf("edit after restore = %v", err)
	}
}

func TestLockedEncounterIsDeletable(t *testing.T) {
	s := gatedService(t)
	e := newIn(t, s, "test")
	_ = s.Locks().Reconcile(ctx, "u1", stubChecker{flag: false})
	if err := s.Delete(ctx, "u1", "u1", e.ID, e.Revision); err != nil {
		t.Fatalf("delete locked = %v", err)
	}
}

func TestEntitlementOutageLocksNothing(t *testing.T) {
	s := gatedService(t)
	e := newIn(t, s, "test")
	if err := s.Locks().Reconcile(ctx, "u1", degraded{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "u1", e.ID); got.LockedAt != nil {
		t.Fatal("locked during an outage")
	}
}

func TestLocksAreOwnerScoped(t *testing.T) {
	s := gatedService(t)
	e := newIn(t, s, "test")
	_ = s.Locks().Reconcile(ctx, "u2", stubChecker{flag: false})
	if got, _ := s.Get(ctx, "u1", e.ID); got.LockedAt != nil {
		t.Fatal("another owner's reconcile locked this encounter")
	}
}

type degraded struct{}

func (degraded) Check(_ context.Context, key string, _ entitlements.Policy) (entitlements.Result, error) {
	return entitlements.Result{Key: key, Degraded: true}, nil
}
