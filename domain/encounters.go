package domain

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
)

// randomID is a combatant ID: 12 random bytes in hex. It is not an ObjectID, whose counter and
// timestamp would let a player see gaps where hidden combatants were added between visible ones.
func randomID() string {
	b := make([]byte, 12)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

// ErrTransition is returned for a status change the encounter's current status does not allow.
// It wraps data.ErrInvalid.
var ErrTransition = fmt.Errorf("%w: status transition not allowed", data.ErrInvalid)

// Encounters implements encounter and combatant rules. Turn order (turn_queue) is computed by the
// ordering engine, not here: adding, restoring, or re-scoring a combatant leaves the queue to it.
type Encounters struct {
	repo    *data.Encounters
	themes  *data.Owned[data.Theme, *data.Theme]
	systems *data.SystemRepo
	newID   func() string
	perm    func(n int) []int
}

// NewEncounters builds the encounter service on db.
func NewEncounters(db store.Database) *Encounters {
	return &Encounters{
		repo:    data.NewEncounters(db),
		themes:  data.NewOwned[data.Theme](db, data.ThemeSpec),
		systems: data.NewSystemRepo(db),
		newID:   randomID,
		perm:    rand.Perm,
	}
}

// checkTheme accepts no theme, a packaged theme, or a custom theme the owner has. Another user's
// theme and a made-up ID are the same refusal, so a probe learns nothing about other accounts.
func checkTheme(ctx context.Context, themes *data.Owned[data.Theme, *data.Theme], owner, id string) error {
	if id == "" || strings.HasPrefix(id, PackagedPrefix) {
		return nil
	}
	if _, err := themes.Get(ctx, owner, id); err != nil {
		if errors.Is(err, data.ErrNotFound) {
			return invalid("theme_id is not one of your themes")
		}
		return err
	}
	return nil
}

// Create stores enc as owner's new draft encounter. The name is required, the game system must
// exist, and any combatants supplied get server-assigned IDs.
func (s *Encounters) Create(ctx context.Context, owner, actor string, enc data.Encounter) (data.Encounter, error) {
	if err := checkTheme(ctx, s.themes, owner, enc.ThemeID); err != nil {
		return data.Encounter{}, err
	}
	enc.Name = strings.TrimSpace(enc.Name)
	if enc.Name == "" {
		return data.Encounter{}, invalid("encounter name is required")
	}
	// An encounter joins a session only through a path that authorizes the caller as that
	// session's GM; until one exists, a caller-supplied session id is refused.
	if enc.SessionID != "" {
		return data.Encounter{}, invalid("an encounter cannot be created inside a session here")
	}
	_, err := s.systems.Get(ctx, enc.GameSystemID)
	if errors.Is(err, data.ErrNotFound) {
		return data.Encounter{}, invalid("the encounter's game system is not defined")
	}
	if err != nil {
		return data.Encounter{}, err
	}
	if err := requireSystem(ctx, enc.GameSystemID); err != nil {
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

// Count returns how many encounters owner holds.
func (s *Encounters) Count(ctx context.Context, owner string) (int64, error) {
	return s.repo.Count(ctx, owner, nil)
}

// UpdateSettings sets the encounter's settings (name, game_system_id, ordering, tie_breaker,
// display flags, theme_id, position). Combat state cannot be set here, but a new ordering, tie
// breaker, or game system recomputes the turn queue in the same write.
func (s *Encounters) UpdateSettings(ctx context.Context, owner, actor, id string, rev int64, set map[string]any) (data.Encounter, error) {
	if name, ok := set["name"].(string); ok {
		set["name"] = strings.TrimSpace(name)
	}
	if theme, ok := set["theme_id"].(string); ok {
		if err := checkTheme(ctx, s.themes, owner, theme); err != nil {
			return data.Encounter{}, err
		}
	}
	if sysID, ok := set["game_system_id"].(string); ok {
		if err := requireSystem(ctx, sysID); err != nil {
			return data.Encounter{}, err
		}
	}
	_, ord := set["ordering"]
	_, tie := set["tie_breaker"]
	_, sys := set["game_system_id"]
	if !ord && !tie && !sys {
		return s.repo.Update(ctx, owner, actor, id, rev, set)
	}
	cur, err := s.repo.Load(ctx, owner, id)
	if err != nil {
		return data.Encounter{}, err
	}
	o, t, sysID := cur.Ordering, cur.TieBreaker, cur.GameSystemID
	if v, ok := set["ordering"].(string); ok {
		o = v
	}
	if v, ok := set["game_system_id"].(string); ok {
		sysID = v
	}
	if v, ok := set["tie_breaker"].(string); ok {
		t = v
	}
	st := stateOf(cur)
	s.recompute(&st, effectiveOrdering(o, sysID), t)
	return s.repo.Write(ctx, owner, actor, id, rev, set, st)
}

func stateOf(cur data.Encounter) data.EncounterState {
	return data.EncounterState{
		Status: cur.Status, Round: cur.Round, CurrentCombatantID: cur.CurrentCombatantID,
		TurnQueue: slices.Clone(cur.TurnQueue), Combatants: slices.Clone(cur.Combatants),
	}
}

// editQueue is edit followed by a turn-queue recompute under the encounter's ordering.
func (s *Encounters) editQueue(ctx context.Context, owner, actor, id string, rev int64, change func(*data.EncounterState, data.Encounter) error) (data.Encounter, error) {
	return s.edit(ctx, owner, actor, id, rev, func(st *data.EncounterState, cur data.Encounter) error {
		if err := change(st, cur); err != nil {
			return err
		}
		s.recompute(st, effectiveOrdering(cur.Ordering, cur.GameSystemID), cur.TieBreaker)
		return nil
	})
}

// Delete soft-deletes the encounter.
func (s *Encounters) Delete(ctx context.Context, owner, actor, id string, rev int64) error {
	return s.repo.SoftDelete(ctx, owner, actor, id, rev)
}

// edit reads the encounter, applies change to its state, and writes the result compare-and-set on
// rev. A missed read is classified 403/404 like the write paths.
// change also receives the encounter as read, for its settings.
func (s *Encounters) edit(ctx context.Context, owner, actor, id string, rev int64, change func(*data.EncounterState, data.Encounter) error) (data.Encounter, error) {
	cur, err := s.repo.Load(ctx, owner, id)
	if err != nil {
		return data.Encounter{}, err
	}
	st := stateOf(cur)
	if err := change(&st, cur); err != nil {
		return data.Encounter{}, err
	}
	return s.repo.SetState(ctx, owner, actor, id, rev, st)
}

// FindByExternalRef returns owner's combatants whose external_refs map key to value.
func (s *Encounters) FindByExternalRef(ctx context.Context, owner, key, value string) ([]data.RefMatch, error) {
	return s.repo.FindByExternalRef(ctx, owner, key, value)
}

// Items is the encounter repository as limit enforcement sees it.
func (s *Encounters) Items() Limited { return s.repo }
