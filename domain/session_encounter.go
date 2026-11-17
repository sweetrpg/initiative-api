package domain

import (
	"context"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// defaultOwnerRecordMaxAge is the default OWNER_ENTITLEMENTS_MAX_AGE, used when
// Sessions.OwnerRecordMaxAge is not set.
const defaultOwnerRecordMaxAge = 7 * 24 * time.Hour

// ownerRecordMaxAge bounds how old the GM's entitlements recorded on a session may be when a
// co-GM acts on them: Sessions.OwnerRecordMaxAge (the configured OWNER_ENTITLEMENTS_MAX_AGE), or
// seven days when unset. Past it, or with nothing recorded, a co-GM's create fails closed until
// the GM next uses the session.
func (s *Sessions) ownerRecordMaxAge() time.Duration {
	if s.OwnerRecordMaxAge > 0 {
		return s.OwnerRecordMaxAge
	}
	return defaultOwnerRecordMaxAge
}

// EncounterGrant is the outcome of authorizing one encounter creation in a session: who may
// create, the owner it belongs to (the GM), the saved-encounters limit, how to see the result, and
// which game systems the owner is entitled to. Only AuthorizeEncounterCreate makes one.
type EncounterGrant struct {
	// Owner is the GM, who owns the encounter whoever creates it.
	Owner     string
	actor     string
	sessionID string
	limit     int
	decision  data.Decision
	viewer    data.Viewer
	systemOK  func(ctx context.Context, systemID string) error
}

// AuthorizeEncounterCreate decides whether actor may create an encounter in the session. The GM
// always may; a co-GM only while the GM has the setting on, read fresh on every call so turning it
// off takes effect at once. The GM's saved-encounters limit applies with private and session
// encounters counted together. The GM is checked live with their own entitlements; a co-GM is
// checked against the GM's entitlements recorded on the session, failing closed when none are
// recorded or the record is older than the configured maximum age. That covers the limit and the
// game-systems flag, so a co-GM can neither use a system the GM lacks nor be refused one the GM has.
func (s *Sessions) AuthorizeEncounterCreate(ctx context.Context, actor, id string) (EncounterGrant, error) {
	sess, m, d, err := s.as(ctx, actor, id, ActCreateEncounter)
	if err != nil {
		return EncounterGrant{}, err
	}
	if m.Role == data.RoleCoGM && !sess.AllowCoGMCreateEncounter {
		return EncounterGrant{}, data.ErrForbidden
	}
	g := EncounterGrant{Owner: sess.OwnerUserID, actor: actor, sessionID: sess.ID, decision: d}
	if g.limit, g.systemOK, err = s.ownerEntitlements(ctx, sess, m.Role); err != nil {
		return EncounterGrant{}, err
	}
	n, err := s.enc.CountAll(ctx, sess.OwnerUserID)
	if err != nil {
		return EncounterGrant{}, err
	}
	if n >= int64(g.limit) {
		return EncounterGrant{}, &EntitlementError{Key: KeySavedEncounters, Limit: g.limit}
	}
	if g.viewer, err = s.repo.AuthorizeView(ctx, actor, id); err != nil {
		return EncounterGrant{}, err
	}
	return g, nil
}

// ownerEntitlements returns the saved-encounters limit and the game-system gate for a creator.
func (s *Sessions) ownerEntitlements(ctx context.Context, sess data.Session, role string) (int, func(context.Context, string) error, error) {
	if role == data.RoleGM {
		chk, ok := ctx.Value(checkerKey{}).(Checker)
		if !ok {
			return 0, nil, entitlements.ErrUnavailable
		}
		res, err := chk.Check(ctx, KeySavedEncounters, entitlements.FailClosed)
		return res.Limit, requireSystem, err
	}
	if sess.OwnerLimitsAt == nil || s.now().Sub(*sess.OwnerLimitsAt) > s.ownerRecordMaxAge() {
		return 0, nil, ErrOwnerLimitsUnknown
	}
	limit, err := OwnerLimit(sess, KeySavedEncounters)
	if err != nil {
		return 0, nil, err
	}
	return limit, recordedSystemGate(sess), nil
}

// recordedSystemGate gates game systems on the owner's recorded flag. No System is always allowed.
func recordedSystemGate(sess data.Session) func(context.Context, string) error {
	return func(_ context.Context, systemID string) error {
		if systemID == "" || systemID == data.DefaultSystemID {
			return nil
		}
		flag, ok := sess.OwnerFlags[KeyGameSystemsExtra]
		if !ok {
			return ErrOwnerLimitsUnknown
		}
		if !flag {
			return &EntitlementRequiredError{Key: KeyGameSystemsExtra}
		}
		return nil
	}
}
