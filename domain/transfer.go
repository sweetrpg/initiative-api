package domain

import (
	"context"
	"maps"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
)

// CreateEncounterFromRoster stores enc as owner's new encounter with one combatant per roster
// character, copying name, kind, notes, default health, and external refs. The source roster is
// not modified, and enc's own combatants are replaced.
func (s *Rosters) CreateEncounterFromRoster(ctx context.Context, owner, actor, rosterID string, enc data.Encounter) (data.Encounter, error) {
	roster, err := s.rosters.Get(ctx, owner, rosterID)
	if err != nil {
		return data.Encounter{}, err
	}
	enc.Combatants = make([]data.Combatant, 0, len(roster.Characters))
	for _, c := range roster.Characters {
		enc.Combatants = append(enc.Combatants, data.Combatant{
			ID: s.newID(), Name: c.Name, Kind: c.Kind, Notes: c.Notes,
			Health: maps.Clone(c.Health), ExternalRefs: maps.Clone(c.ExternalRefs),
			Conditions: []data.Condition{}, SourceCharacterID: c.ID,
		})
	}
	if err := s.encounters.Create(ctx, owner, actor, &enc); err != nil {
		return data.Encounter{}, err
	}
	return enc, nil
}

// SaveAsRoster stores a new roster holding one character per combatant of owner's encounter that
// is not removed. Current health becomes the character's defaults. The encounter is not modified.
// An empty name falls back to the encounter's name.
func (s *Rosters) SaveAsRoster(ctx context.Context, owner, actor, encounterID, name string) (data.Roster, error) {
	enc, err := s.encounters.Get(ctx, owner, encounterID)
	if err != nil {
		return data.Roster{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = enc.Name
	}
	chars := make([]data.Character, 0, len(enc.Combatants))
	for _, c := range enc.Combatants {
		if c.Removed {
			continue
		}
		chars = append(chars, data.Character{
			Name: c.Name, Kind: c.Kind, Notes: c.Notes, Health: c.Health, ExternalRefs: c.ExternalRefs,
		})
	}
	return s.Create(ctx, owner, actor, name, false, chars)
}
