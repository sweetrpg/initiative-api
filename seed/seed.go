// Package seed loads the versioned game-system definitions checked into this repo. Loading is
// idempotent: unchanged definitions are skipped and keep their identifiers.
package seed

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/sweetrpg/initiative-api/data"
)

//go:embed systems/families.json systems/systems.json
var files embed.FS

// Upserter is the part of data.SystemRepo the loader needs.
type Upserter interface {
	Upsert(ctx context.Context, sys data.GameSystem) (data.Outcome, error)
}

// Result counts what a load did.
type Result struct{ Created, Updated, Unchanged int }

type systemFile struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Family  string              `json:"family"`
	Aliases []string            `json:"aliases"`
	Gated   bool                `json:"gated"`
	Fields  []data.TrackerField `json:"fields"` // optional override of the family's fields
}

// Definitions parses the seed files and expands each system's tracker fields from its family
// (a system may carry its own fields instead). Every result passes data.ValidateSystem.
func Definitions() ([]data.GameSystem, error) {
	var families map[string][]data.TrackerField
	if err := readJSON("systems/families.json", &families); err != nil {
		return nil, err
	}
	var systems []systemFile
	if err := readJSON("systems/systems.json", &systems); err != nil {
		return nil, err
	}
	return expand(families, systems)
}

// expand builds and validates the systems, and checks identifiers across the whole set: IDs are
// unique, and no alias repeats another alias or equals any system's ID (an alias lookup must
// never be ambiguous with an ID lookup).
func expand(families map[string][]data.TrackerField, systems []systemFile) ([]data.GameSystem, error) {
	out := make([]data.GameSystem, 0, len(systems))
	for _, s := range systems {
		fields := s.Fields
		if len(fields) == 0 {
			var ok bool
			if fields, ok = families[s.Family]; !ok {
				return nil, fmt.Errorf("seed: system %q uses unknown family %q", s.ID, s.Family)
			}
		}
		aliases := s.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		sys := data.GameSystem{ID: s.ID, Name: s.Name, Family: s.Family, Aliases: aliases, Gated: s.Gated, Fields: fields}
		if err := data.ValidateSystem(sys); err != nil {
			return nil, err
		}
		out = append(out, sys)
	}
	return out, checkIdentifiers(out)
}

func checkIdentifiers(systems []data.GameSystem) error {
	owner := map[string]string{} // identifier -> system that claims it
	for _, s := range systems {
		if prev, dup := owner[s.ID]; dup {
			return fmt.Errorf("seed: id %q used by both %q and %q", s.ID, prev, s.ID)
		}
		owner[s.ID] = s.ID
	}
	for _, s := range systems {
		for _, a := range s.Aliases {
			if prev, dup := owner[a]; dup {
				return fmt.Errorf("seed: alias %q of %q collides with %q", a, s.ID, prev)
			}
			owner[a] = s.ID
		}
	}
	return nil
}

func readJSON(name string, v any) error {
	b, err := files.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("seed: parse %s: %w", name, err)
	}
	return nil
}

// Load upserts every seeded definition and reports the outcomes. It stops at the first error.
func Load(ctx context.Context, repo Upserter) (Result, error) {
	defs, err := Definitions()
	if err != nil {
		return Result{}, err
	}
	var r Result
	for _, d := range defs {
		o, err := repo.Upsert(ctx, d)
		if err != nil {
			return r, fmt.Errorf("seed: %s: %w", d.ID, err)
		}
		switch o {
		case data.Created:
			r.Created++
		case data.Updated:
			r.Updated++
		default:
			r.Unchanged++
		}
	}
	return r, nil
}
