// Package domain holds initiative-api's rules over the owner-scoped data layer: rosters and
// characters, and moving cast between rosters and encounters. All access goes through data's
// owner-scoped repositories, so every call carries the caller's user ID.
package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var kinds = data.Kinds

// Rosters implements roster and character rules.
type Rosters struct {
	rosters    *data.Owned[data.Roster, *data.Roster]
	encounters *data.Encounters
	newID      func() string
}

// NewRosters builds the roster service on db.
func NewRosters(db store.Database) *Rosters {
	return &Rosters{
		rosters:    data.NewOwned[data.Roster](db, data.RosterSpec),
		encounters: data.NewEncounters(db),
		newID:      func() string { return primitive.NewObjectID().Hex() },
	}
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", data.ErrInvalid, fmt.Sprintf(format, a...))
}

// normalize trims and validates a character, copying its maps so the stored value does not alias
// the caller's. The ID is left for the caller to assign.
func normalize(c data.Character) (data.Character, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return c, invalid("character name is required")
	}
	if !slices.Contains(kinds, c.Kind) {
		return c, invalid("character kind %q", c.Kind)
	}
	c.Health, c.ExternalRefs = maps.Clone(c.Health), maps.Clone(c.ExternalRefs)
	return c, nil
}

func (s *Rosters) prepare(chars []data.Character) ([]data.Character, error) {
	out := make([]data.Character, 0, len(chars))
	for _, c := range chars {
		n, err := normalize(c)
		if err != nil {
			return nil, err
		}
		n.ID = s.newID()
		out = append(out, n)
	}
	return out, nil
}

// Create stores a new roster for owner. Character IDs are assigned here; any supplied are ignored.
func (s *Rosters) Create(ctx context.Context, owner, actor, name string, favorite bool, chars []data.Character) (data.Roster, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return data.Roster{}, invalid("roster name is required")
	}
	prepared, err := s.prepare(chars)
	if err != nil {
		return data.Roster{}, err
	}
	r := data.Roster{Name: name, Favorite: favorite, Characters: prepared}
	if err := s.rosters.Create(ctx, owner, actor, &r); err != nil {
		return data.Roster{}, err
	}
	return r, nil
}

// Get returns owner's roster.
func (s *Rosters) Get(ctx context.Context, owner, id string) (data.Roster, error) {
	return s.rosters.Get(ctx, owner, id)
}

// List returns a page of owner's rosters.
func (s *Rosters) List(ctx context.Context, owner string, q data.Query) (data.Page[data.Roster], error) {
	return s.rosters.List(ctx, owner, q)
}

// Count returns how many rosters owner holds.
func (s *Rosters) Count(ctx context.Context, owner string) (int64, error) {
	return s.rosters.Count(ctx, owner, nil)
}

// Rename sets the roster's name and favorite flag, compare-and-set on revision.
func (s *Rosters) Rename(ctx context.Context, owner, actor, id string, rev int64, name string, favorite bool) (data.Roster, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return data.Roster{}, invalid("roster name is required")
	}
	return s.rosters.Update(ctx, owner, actor, id, rev, map[string]any{"name": name, "favorite": favorite})
}

// Delete soft-deletes the roster. Encounters created from it keep their combatants.
func (s *Rosters) Delete(ctx context.Context, owner, actor, id string, rev int64) error {
	return s.rosters.SoftDelete(ctx, owner, actor, id, rev)
}

// editCharacters applies edit to a copy of the roster's characters and writes the result
// compare-and-set on rev.
func (s *Rosters) editCharacters(ctx context.Context, owner, actor, id string, rev int64, edit func([]data.Character) ([]data.Character, error)) (data.Roster, error) {
	cur, err := s.rosters.Get(ctx, owner, id)
	if errors.Is(err, data.ErrNotFound) {
		// The read is only needed to build the new character list; classify a miss as 403 or 404
		// like the write paths do.
		if cerr := s.rosters.Missing(ctx, owner, id); cerr != nil {
			return data.Roster{}, cerr
		}
	}
	if err != nil {
		return data.Roster{}, err
	}
	next, err := edit(slices.Clone(cur.Characters))
	if err != nil {
		return data.Roster{}, err
	}
	return s.rosters.Update(ctx, owner, actor, id, rev, map[string]any{"characters": next})
}

func indexOf(chars []data.Character, id string) int {
	return slices.IndexFunc(chars, func(c data.Character) bool { return c.ID == id })
}

// AddCharacter appends c to the roster with a new ID.
func (s *Rosters) AddCharacter(ctx context.Context, owner, actor, id string, rev int64, c data.Character) (data.Roster, error) {
	n, err := normalize(c)
	if err != nil {
		return data.Roster{}, err
	}
	n.ID = s.newID()
	return s.editCharacters(ctx, owner, actor, id, rev, func(cs []data.Character) ([]data.Character, error) {
		return append(cs, n), nil
	})
}

// UpdateCharacter replaces the roster character with ID charID, keeping its ID.
func (s *Rosters) UpdateCharacter(ctx context.Context, owner, actor, id string, rev int64, charID string, c data.Character) (data.Roster, error) {
	n, err := normalize(c)
	if err != nil {
		return data.Roster{}, err
	}
	n.ID = charID
	return s.editCharacters(ctx, owner, actor, id, rev, func(cs []data.Character) ([]data.Character, error) {
		i := indexOf(cs, charID)
		if i < 0 {
			return nil, data.ErrNotFound
		}
		cs[i] = n
		return cs, nil
	})
}

// RemoveCharacter deletes the roster character with ID charID.
func (s *Rosters) RemoveCharacter(ctx context.Context, owner, actor, id string, rev int64, charID string) (data.Roster, error) {
	return s.editCharacters(ctx, owner, actor, id, rev, func(cs []data.Character) ([]data.Character, error) {
		i := indexOf(cs, charID)
		if i < 0 {
			return nil, data.ErrNotFound
		}
		return slices.Delete(cs, i, i+1), nil
	})
}

// Items is the roster repository as limit enforcement sees it.
func (s *Rosters) Items() Limited { return s.rosters }
