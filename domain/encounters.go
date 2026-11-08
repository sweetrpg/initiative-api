package domain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ErrTransition is returned for a status change the encounter's current status does not allow.
// It wraps data.ErrInvalid.
var ErrTransition = fmt.Errorf("%w: status transition not allowed", data.ErrInvalid)

// Encounters implements encounter and combatant rules. Turn order (turn_queue) is computed by the
// ordering engine, not here: adding, restoring, or re-scoring a combatant leaves the queue to it.
type Encounters struct {
	repo    *data.Encounters
	systems *data.SystemRepo
	newID   func() string
}

// NewEncounters builds the encounter service on db.
func NewEncounters(db store.Database) *Encounters {
	return &Encounters{
		repo:    data.NewEncounters(db),
		systems: data.NewSystemRepo(db),
		newID:   func() string { return primitive.NewObjectID().Hex() },
	}
}

// Create stores enc as owner's new draft encounter. The name is required, the game system must
// exist, and any combatants supplied get server-assigned IDs.
func (s *Encounters) Create(ctx context.Context, owner, actor string, enc data.Encounter) (data.Encounter, error) {
	enc.Name = strings.TrimSpace(enc.Name)
	if enc.Name == "" {
		return data.Encounter{}, invalid("encounter name is required")
	}
	if _, err := s.systems.Get(ctx, enc.GameSystemID); err != nil {
		if errors.Is(err, data.ErrNotFound) {
			return data.Encounter{}, invalid("unknown game system %q", enc.GameSystemID)
		}
		return data.Encounter{}, err
	}
	combatants := make([]data.Combatant, 0, len(enc.Combatants))
	for _, c := range enc.Combatants {
		n, err := newCombatant(c, s.newID())
		if err != nil {
			return data.Encounter{}, err
		}
		combatants = append(combatants, n)
	}
	enc.Combatants = combatants
	if err := s.repo.Create(ctx, owner, actor, &enc); err != nil {
		return data.Encounter{}, err
	}
	return enc, nil
}

// Get returns owner's encounter.
func (s *Encounters) Get(ctx context.Context, owner, id string) (data.Encounter, error) {
	return s.repo.Get(ctx, owner, id)
}

// List returns a page of owner's encounters.
func (s *Encounters) List(ctx context.Context, owner string, q data.Query) (data.Page[data.Encounter], error) {
	return s.repo.List(ctx, owner, q)
}

// UpdateSettings sets the encounter's settings (name, game_system_id, ordering, tie_breaker,
// display flags, theme_id, position). Combat state cannot be set here.
func (s *Encounters) UpdateSettings(ctx context.Context, owner, actor, id string, rev int64, set map[string]any) (data.Encounter, error) {
	return s.repo.Update(ctx, owner, actor, id, rev, set)
}

// Delete soft-deletes the encounter.
func (s *Encounters) Delete(ctx context.Context, owner, actor, id string, rev int64) error {
	return s.repo.SoftDelete(ctx, owner, actor, id, rev)
}

// edit reads the encounter, applies change to its state, and writes the result compare-and-set on
// rev. A missed read is classified 403/404 like the write paths.
func (s *Encounters) edit(ctx context.Context, owner, actor, id string, rev int64, change func(*data.EncounterState) error) (data.Encounter, error) {
	cur, err := s.repo.Get(ctx, owner, id)
	if errors.Is(err, data.ErrNotFound) {
		if cerr := s.repo.Missing(ctx, owner, id); cerr != nil {
			return data.Encounter{}, cerr
		}
	}
	if err != nil {
		return data.Encounter{}, err
	}
	st := data.EncounterState{
		Status: cur.Status, Round: cur.Round, CurrentCombatantID: cur.CurrentCombatantID,
		TurnQueue: slices.Clone(cur.TurnQueue), Combatants: slices.Clone(cur.Combatants),
	}
	if err := change(&st); err != nil {
		return data.Encounter{}, err
	}
	return s.repo.SetState(ctx, owner, actor, id, rev, st)
}
