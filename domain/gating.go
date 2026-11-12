package domain

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// KeyGameSystemsExtra is the flag that unlocks every game system other than No System.
const KeyGameSystemsExtra = "initiative.game_systems_extra"

// EntitlementRequiredError is the typed denial for a flag the caller does not hold.
type EntitlementRequiredError struct{ Key string }

func (e *EntitlementRequiredError) Error() string {
	return fmt.Sprintf("domain: entitlement %s is required", e.Key)
}

type checkerKey struct{}

// WithChecker attaches the calling user's entitlement checker to ctx, so rules deep in the domain
// (a system switch inside a settings update) can check without every signature carrying it. The
// server sets it per request, bound to that user's own bearer (PADR-0037).
func WithChecker(ctx context.Context, c Checker) context.Context {
	return context.WithValue(ctx, checkerKey{}, c)
}

// requireSystem rejects use of a gated game system by a caller without the flag. No System is
// free. Creates and switches fail closed: with no checker in ctx or entitlements unreachable the
// call is refused.
func requireSystem(ctx context.Context, systemID string) error {
	if systemID == "" || systemID == data.DefaultSystemID {
		return nil
	}
	chk, ok := ctx.Value(checkerKey{}).(Checker)
	if !ok {
		return entitlements.ErrUnavailable
	}
	res, err := chk.Check(ctx, KeyGameSystemsExtra, entitlements.FailClosed)
	if err != nil {
		return err
	}
	if !res.Flag {
		return &EntitlementRequiredError{Key: KeyGameSystemsExtra}
	}
	return nil
}

// Locks keeps encounters that use a gated game system in step with the owner's flag: locked while
// it is missing, restored unchanged when it returns.
type Locks struct {
	repo *data.Encounters
}

// Locks returns the lock reconciler for these encounters.
func (s *Encounters) Locks() *Locks { return &Locks{repo: s.repo} }

// Reconcile reads the flag and locks or unlocks owner's encounters to match. When entitlements
// cannot be read it changes nothing, so an outage never locks anyone out.
func (l *Locks) Reconcile(ctx context.Context, owner string, chk Checker) error {
	res, err := chk.Check(ctx, KeyGameSystemsExtra, entitlements.FailOpen)
	if err != nil {
		return err
	}
	if res.Degraded {
		return nil
	}
	var n int
	if res.Flag {
		n, err = l.repo.UnlockAll(ctx, owner)
	} else {
		n, err = l.repo.LockGated(ctx, owner)
	}
	if n > 0 {
		slog.InfoContext(ctx, "game-system locks changed", "locked", !res.Flag, "count", n)
	}
	return err
}
