package domain

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
)

// CombatantPatch is a partial combatant edit: nil fields are left alone. Health values are merged
// over the stored map (keys not in the patch keep their values); the merged map is what gets
// validated.
type CombatantPatch struct {
	Name       *string
	Kind       *string
	Initiative *float64
	Notes      *string
	Health     map[string]int
}

func newCombatant(c data.Combatant, id string) (data.Combatant, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return c, invalid("combatant name is required")
	}
	if !slices.Contains(kinds, c.Kind) {
		return c, invalid("combatant kind %q", c.Kind)
	}
	c.ID, c.Removed = id, false
	c.Health, c.ExternalRefs = maps.Clone(c.Health), maps.Clone(c.ExternalRefs)
	c.Conditions = slices.Clone(c.Conditions)
	if c.Conditions == nil {
		c.Conditions = []data.Condition{}
	}
	return c, nil
}

func findCombatant(st *data.EncounterState, id string) (*data.Combatant, error) {
	i := slices.IndexFunc(st.Combatants, func(c data.Combatant) bool { return c.ID == id })
	if i < 0 {
		return nil, data.ErrNotFound
	}
	return &st.Combatants[i], nil
}

// AddCombatant appends c to the encounter with a new ID.
func (s *Encounters) AddCombatant(ctx context.Context, owner, actor, id string, rev int64, c data.Combatant) (data.Encounter, error) {
	n, err := newCombatant(c, s.newID())
	if err != nil {
		return data.Encounter{}, err
	}
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		st.Combatants = append(st.Combatants, n)
		return nil
	})
}

// UpdateCombatant applies patch to the combatant with ID combatantID.
func (s *Encounters) UpdateCombatant(ctx context.Context, owner, actor, id string, rev int64, combatantID string, patch CombatantPatch) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		c, err := findCombatant(st, combatantID)
		if err != nil {
			return err
		}
		return applyPatch(c, patch)
	})
}

func applyPatch(c *data.Combatant, p CombatantPatch) error {
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return invalid("combatant name is required")
		}
		c.Name = name
	}
	if p.Kind != nil {
		if !slices.Contains(kinds, *p.Kind) {
			return invalid("combatant kind %q", *p.Kind)
		}
		c.Kind = *p.Kind
	}
	if p.Initiative != nil {
		v := *p.Initiative
		c.Initiative = &v
	}
	if p.Notes != nil {
		c.Notes = *p.Notes
	}
	if p.Health != nil {
		merged := maps.Clone(c.Health)
		if merged == nil {
			merged = map[string]int{}
		}
		maps.Copy(merged, p.Health)
		c.Health = merged
	}
	return nil
}

// RemoveCombatant marks the combatant removed. It stays in the record, leaves the turn queue, and
// stops being current if it was.
func (s *Encounters) RemoveCombatant(ctx context.Context, owner, actor, id string, rev int64, combatantID string) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		c, err := findCombatant(st, combatantID)
		if err != nil {
			return err
		}
		c.Removed = true
		st.TurnQueue = slices.DeleteFunc(st.TurnQueue, func(q string) bool { return q == combatantID })
		if st.CurrentCombatantID == combatantID {
			st.CurrentCombatantID = ""
		}
		return nil
	})
}

// RestoreCombatant clears the removed mark. The ordering engine puts it back in the queue.
func (s *Encounters) RestoreCombatant(ctx context.Context, owner, actor, id string, rev int64, combatantID string) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, _ string) error {
		c, err := findCombatant(st, combatantID)
		if err != nil {
			return err
		}
		c.Removed = false
		return nil
	})
}
