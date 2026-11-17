package domain

import (
	"context"
	"errors"
	"slices"

	"github.com/sweetrpg/initiative-api/data"
)

// controllerRoles are the member roles that may control a combatant. An observer has no control
// over any combatant (initiative-player-access), so assigning one is refused.
var controllerRoles = []string{data.RoleGM, data.RoleCoGM, data.RolePlayer}

// AssignControllers sets the controllers of one combatant of a session's encounter, at the
// encounter revision rev, and returns the encounter as the caller (a GM or co-GM) sees it. The
// list may be empty. Every listed user must hold a live member record in the session with a
// controlling role, checked now against the session's records; a non-member or an observer is an
// invalid argument and nothing is written. The write is one atomic compare-and-set on the
// revision (data.Encounters.UpdateInSession).
func (s *Encounters) AssignControllers(ctx context.Context, sessions *Sessions, actor, sessionID, encounterID, combatantID string, userIDs []string, rev int64) (EncounterView, error) {
	_, _, d, err := sessions.as(ctx, actor, sessionID, ActManageMembers)
	if err != nil {
		return nil, err
	}
	viewer, err := sessions.Viewer(ctx, actor, sessionID)
	if err != nil {
		return nil, err
	}
	if len(userIDs) > data.MaxCombatants {
		return nil, invalid("at most %d controllers", data.MaxCombatants)
	}
	ids, err := sessions.controllersOf(ctx, d, userIDs)
	if err != nil {
		return nil, err
	}
	enc, err := s.repo.LoadForViewer(ctx, viewer, encounterID)
	if err != nil {
		return nil, err
	}
	if enc.Revision != rev {
		return nil, data.ErrRevisionConflict
	}
	cs := slices.Clone(enc.Combatants)
	i := slices.IndexFunc(cs, func(c data.Combatant) bool { return c.ID == combatantID && !c.Removed })
	if i < 0 {
		return nil, data.ErrNotFound
	}
	cs[i].ControllerUserIDs = ids
	updated, err := s.repo.UpdateInSession(ctx, d, encounterID, rev, map[string]any{"combatants": cs})
	if err != nil {
		return nil, err
	}
	// A listed user may have left between the membership check and the write. If so, scrub them
	// and report a conflict so the GM reassigns against the new membership.
	if gone := sessions.notControllers(ctx, d, ids); len(gone) > 0 {
		for _, u := range gone {
			sessions.scrubControllers(ctx, d, u)
		}
		return nil, data.ErrConflict
	}
	return s.render(ctx, updated, viewer)
}

// notControllers returns the users in ids who no longer hold a controlling membership. A lookup
// that fails for any reason counts as "gone": the user is then scrubbed, which a legitimate
// controller can undo by being assigned again, whereas keeping an unverified id could not be.
func (s *Sessions) notControllers(ctx context.Context, d data.Decision, ids []string) []string {
	var gone []string
	for _, u := range ids {
		m, err := s.repo.Member(ctx, d, u)
		if err != nil || !slices.Contains(controllerRoles, m.Role) {
			gone = append(gone, u)
		}
	}
	return gone
}

// controllersOf validates userIDs against the session's live member records and returns them
// de-duplicated, in order. nil for an empty list.
func (s *Sessions) controllersOf(ctx context.Context, d data.Decision, userIDs []string) ([]string, error) {
	var out []string
	for _, u := range userIDs {
		if slices.Contains(out, u) {
			continue
		}
		m, err := s.repo.Member(ctx, d, u)
		if err != nil && !isNotFoundOrInvalid(err) {
			return nil, err
		}
		if err != nil {
			return nil, invalid("%q is not a member of this session", u)
		}
		if !slices.Contains(controllerRoles, m.Role) {
			return nil, invalid("%q cannot control a combatant: an observer has no control", u)
		}
		out = append(out, u)
	}
	return out, nil
}

func isNotFoundOrInvalid(err error) bool {
	return errors.Is(err, data.ErrNotFound) || errors.Is(err, data.ErrInvalid)
}
