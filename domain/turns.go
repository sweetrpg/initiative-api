package domain

import (
	"context"
	"fmt"
	"slices"

	"github.com/sweetrpg/initiative-api/data"
)

// recompute rebuilds the turn queue under ordering and tie. In an active encounter it also keeps
// a current combatant: if the current one left the queue, the combatant now at its old position
// takes the turn (wrapping to the front and starting the next round if it was last), and an empty
// current picks the first.
func (s *Encounters) recompute(st *data.EncounterState, ordering, tie string) {
	prev := st.TurnQueue
	st.TurnQueue = buildQueue(ordering, tie, st.Combatants, prev, s.perm)
	if st.Status != data.StatusActive || slices.Contains(st.TurnQueue, st.CurrentCombatantID) {
		return
	}
	if len(st.TurnQueue) == 0 {
		st.CurrentCombatantID = ""
		return
	}
	old := slices.Index(prev, st.CurrentCombatantID)
	pos := max(old, 0)
	if old >= len(st.TurnQueue) {
		st.Round++ // the removed combatant was last: the turn wraps to the front, a new round
	}
	st.CurrentCombatantID = st.TurnQueue[pos%len(st.TurnQueue)]
	beginTurn(st)
}

// beginTurn ticks the conditions of the current combatant.
func beginTurn(st *data.EncounterState) {
	i := slices.IndexFunc(st.Combatants, func(c data.Combatant) bool { return c.ID == st.CurrentCombatantID })
	if i >= 0 {
		expireConditions(&st.Combatants[i])
	}
}

func queuedTurn(st *data.EncounterState) (pos int, err error) {
	if st.Status != data.StatusActive {
		return 0, fmt.Errorf("%w: encounter is %s", ErrTransition, st.Status)
	}
	if len(st.TurnQueue) == 0 {
		return 0, invalid("the turn queue is empty")
	}
	return slices.Index(st.TurnQueue, st.CurrentCombatantID), nil
}

// Advance passes the turn to the next combatant in the queue. Past the last entry it wraps to the
// first and starts the next round. The new current combatant's timed conditions lose one round,
// and expire at zero.
func (s *Encounters) Advance(ctx context.Context, owner, actor, id string, rev int64) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ data.Encounter) error {
		pos, err := queuedTurn(st)
		if err != nil {
			return err
		}
		idx, wrapped := nextTurn(pos, len(st.TurnQueue))
		if wrapped {
			st.Round++
		}
		st.CurrentCombatantID = st.TurnQueue[idx]
		beginTurn(st)
		return nil
	})
}

// StepBack returns the turn to the previous combatant. Stepping back past the first entry goes to
// the last and the previous round; it is rejected on the first turn of round 1. Conditions that
// already ticked are not restored.
func (s *Encounters) StepBack(ctx context.Context, owner, actor, id string, rev int64) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ data.Encounter) error {
		pos, err := queuedTurn(st)
		if err != nil {
			return err
		}
		idx, wrapped := prevTurn(pos, len(st.TurnQueue))
		if wrapped {
			if st.Round <= 1 {
				return fmt.Errorf("%w: already at the first turn", ErrTransition)
			}
			st.Round--
		}
		st.CurrentCombatantID = st.TurnQueue[idx]
		return nil
	})
}
