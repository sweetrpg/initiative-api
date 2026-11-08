package data

import (
	"context"
	"fmt"
	"slices"

	"github.com/sweetrpg/initiative-api/store"
)

var (
	orderings   = []string{OrderHighToLow, OrderLowToHigh, OrderPCVsAdversary, OrderPlayerManaged}
	tieBreakers = []string{TieRandom, TieAlphabetical, TieQuery}
	statuses    = []string{StatusDraft, StatusActive, StatusEnded}
)

// Encounters is the owner-scoped encounter repository. It validates enumerated values and
// queue references before any write reaches the collection.
type Encounters struct {
	*Owned[Encounter, *Encounter]
}

// NewEncounters builds the encounter repository for db.
func NewEncounters(db store.Database) *Encounters {
	return &Encounters{NewOwned[Encounter](db, EncounterSpec)}
}

// ValidateEncounter checks a whole encounter document: ordering, tie breaker, and status come
// from their constants, and every turn_queue entry names one of the encounter's combatants.
func ValidateEncounter(e *Encounter) error {
	if !slices.Contains(orderings, e.Ordering) {
		return fmt.Errorf("%w: ordering %q", ErrInvalid, e.Ordering)
	}
	if !slices.Contains(tieBreakers, e.TieBreaker) {
		return fmt.Errorf("%w: tie_breaker %q", ErrInvalid, e.TieBreaker)
	}
	if !slices.Contains(statuses, e.Status) {
		return fmt.Errorf("%w: status %q", ErrInvalid, e.Status)
	}
	ids := make(map[string]bool, len(e.Combatants))
	for _, c := range e.Combatants {
		if ids[c.ID] {
			return fmt.Errorf("%w: duplicate combatant id %q", ErrInvalid, c.ID)
		}
		ids[c.ID] = true
	}
	for _, id := range e.TurnQueue {
		if !ids[id] {
			return fmt.Errorf("%w: turn_queue entry %q is not a combatant", ErrInvalid, id)
		}
	}
	return nil
}

func validateEncounterSet(set map[string]any) error {
	if v, ok := set["ordering"]; ok {
		if s, isStr := v.(string); !isStr || !slices.Contains(orderings, s) {
			return fmt.Errorf("%w: ordering %v", ErrInvalid, v)
		}
	}
	if v, ok := set["tie_breaker"]; ok {
		if s, isStr := v.(string); !isStr || !slices.Contains(tieBreakers, s) {
			return fmt.Errorf("%w: tie_breaker %v", ErrInvalid, v)
		}
	}
	return nil
}

// Create resets e's combat state to a fresh draft, validates it, then stores it as owner's new
// encounter. Callers cannot create an encounter that is already running.
func (r *Encounters) Create(ctx context.Context, ownerUserID, actingUserID string, e *Encounter) error {
	e.Status, e.Round, e.CurrentCombatantID, e.TurnQueue = StatusDraft, 0, "", nil
	if err := ValidateEncounter(e); err != nil {
		return err
	}
	return r.Owned.Create(ctx, ownerUserID, actingUserID, e)
}

// Update validates the enumerated values in set, then applies it with the revision check.
func (r *Encounters) Update(ctx context.Context, ownerUserID, actingUserID, id string, expectedRevision int64, set map[string]any) (Encounter, error) {
	if err := validateEncounterSet(set); err != nil {
		return Encounter{}, err
	}
	return r.Owned.Update(ctx, ownerUserID, actingUserID, id, expectedRevision, set)
}
