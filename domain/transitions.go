package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/sweetrpg/initiative-api/data"
)

// Start moves a draft encounter to active at round 1. The current combatant is chosen by the
// turn engine once the queue exists.
func (s *Encounters) Start(ctx context.Context, owner, actor, id string, rev int64) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		if st.Status != data.StatusDraft {
			return fmt.Errorf("%w: cannot start from %s", ErrTransition, st.Status)
		}
		st.Status, st.Round = data.StatusActive, 1
		return nil
	})
}

// End moves an active encounter to ended.
func (s *Encounters) End(ctx context.Context, owner, actor, id string, rev int64) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		if st.Status != data.StatusActive {
			return fmt.Errorf("%w: cannot end from %s", ErrTransition, st.Status)
		}
		st.Status = data.StatusEnded
		return nil
	})
}

// Reset returns the encounter to draft at round 0 with no current combatant and an empty turn queue, clears every
// combatant's conditions, and sets health values back to the game system's defaults. Combatants,
// their initiative, and removed marks are kept.
func (s *Encounters) Reset(ctx context.Context, owner, actor, id string, rev int64) (data.Encounter, error) {
	cur, err := s.repo.Load(ctx, owner, id)
	if err != nil {
		return data.Encounter{}, err
	}
	sys, err := s.systems.Get(ctx, cur.GameSystemID)
	if errors.Is(err, data.ErrNotFound) {
		return data.Encounter{}, invalid("unknown game system %q", cur.GameSystemID)
	}
	if err != nil {
		return data.Encounter{}, err
	}
	defaults := make(map[string]int, len(sys.Fields))
	for _, f := range sys.Fields {
		defaults[f.Key] = f.Def
	}
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		st.Status, st.Round, st.CurrentCombatantID, st.TurnQueue = data.StatusDraft, 0, "", nil
		for i := range st.Combatants {
			c := &st.Combatants[i]
			c.Conditions = []data.Condition{}
			c.Health = maps.Clone(c.Health)
			for k := range c.Health {
				if d, ok := defaults[k]; ok {
					c.Health[k] = d
				}
			}
		}
		return nil
	})
}
