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
	ids, err := sessions.controllersOf(ctx, d, userIDs)
	if err != nil {
		return nil, err
	}
	enc, err := s.repo.LoadForViewer(ctx, viewer, encounterID)
	if err != nil {
		return nil, err
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
	return s.render(ctx, updated, viewer)
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
