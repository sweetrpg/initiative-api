package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

var ctx = context.Background()

func newService(t *testing.T) *Rosters {
	t.Helper()
	s := NewRosters(fake.NewDB())
	n := 0
	s.newID = func() string { n++; return "id" + string(rune('0'+n)) }
	return s
}

func goblin() data.Character {
	return data.Character{Name: " Goblin ", Kind: data.KindAdversary, Notes: "sneaky", Health: map[string]int{"hp": 7}}
}

func mustRoster(t *testing.T, s *Rosters, owner string, chars ...data.Character) data.Roster {
	t.Helper()
	r, err := s.Create(ctx, owner, owner, "Cast", false, chars)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateAssignsCharacterIDsAndTrimsNames(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1", goblin(), goblin())
	if r.Characters[0].Name != "Goblin" || r.Characters[0].ID == "" || r.Characters[0].ID == r.Characters[1].ID {
		t.Fatalf("characters = %+v, want trimmed names and distinct IDs", r.Characters)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	tests := []struct {
		name  string
		title string
		chars []data.Character
	}{
		{"empty roster name", " ", nil},
		{"empty character name", "Cast", []data.Character{{Name: " ", Kind: data.KindPC}}},
		{"bad kind", "Cast", []data.Character{{Name: "X", Kind: "dragon"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newService(t).Create(ctx, "u1", "u1", tt.title, false, tt.chars); !errors.Is(err, data.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRosterIsOwnerScoped(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1")
	if _, err := s.Get(ctx, "u2", r.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("get err = %v, want ErrNotFound", err)
	}
	if _, err := s.AddCharacter(ctx, "u2", "u2", r.ID, 1, goblin()); !errors.Is(err, data.ErrForbidden) {
		t.Fatalf("add err = %v, want ErrForbidden", err)
	}
}

func TestCharacterEditsAdvanceRevision(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1")
	r, err := s.AddCharacter(ctx, "u1", "u1", r.ID, r.Revision, goblin())
	if err != nil || len(r.Characters) != 1 || r.Revision != 2 {
		t.Fatalf("add: err=%v roster=%+v", err, r)
	}
	id := r.Characters[0].ID
	r, err = s.UpdateCharacter(ctx, "u1", "u1", r.ID, r.Revision, id, data.Character{Name: "Hobgoblin", Kind: data.KindAdversary})
	if err != nil || r.Characters[0].Name != "Hobgoblin" || r.Characters[0].ID != id || r.Revision != 3 {
		t.Fatalf("update: err=%v roster=%+v", err, r)
	}
	r, err = s.RemoveCharacter(ctx, "u1", "u1", r.ID, r.Revision, id)
	if err != nil || len(r.Characters) != 0 || r.Revision != 4 {
		t.Fatalf("remove: err=%v roster=%+v", err, r)
	}
}

func TestCharacterEditWithStaleRevisionConflicts(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1")
	if _, err := s.AddCharacter(ctx, "u1", "u1", r.ID, 1, goblin()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCharacter(ctx, "u1", "u1", r.ID, 1, goblin()); !errors.Is(err, data.ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
}

func TestMissingCharacterIsNotFound(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1")
	if _, err := s.UpdateCharacter(ctx, "u1", "u1", r.ID, 1, "nope", goblin()); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("update err = %v, want ErrNotFound", err)
	}
	if _, err := s.RemoveCharacter(ctx, "u1", "u1", r.ID, 1, "nope"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("remove err = %v, want ErrNotFound", err)
	}
}

func TestRenameAndDelete(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1")
	r, err := s.Rename(ctx, "u1", "u1", r.ID, 1, "Boss Fight", true)
	if err != nil || r.Name != "Boss Fight" || !r.Favorite {
		t.Fatalf("rename: err=%v roster=%+v", err, r)
	}
	if err := s.Delete(ctx, "u1", "u1", r.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "u1", r.ID); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

func baseEncounter() data.Encounter {
	return data.Encounter{Name: "Ambush", GameSystemID: "dnd5e", Ordering: data.OrderHighToLow, TieBreaker: data.TieRandom}
}

func TestCreateEncounterFromRosterCopiesCharacters(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1", goblin())
	enc, err := s.CreateEncounterFromRoster(ctx, "u1", "u1", r.ID, baseEncounter())
	if err != nil {
		t.Fatal(err)
	}
	c := enc.Combatants[0]
	if len(enc.Combatants) != 1 || c.Name != "Goblin" || c.Notes != "sneaky" || c.Health["hp"] != 7 ||
		c.SourceCharacterID != r.Characters[0].ID || enc.Status != data.StatusDraft {
		t.Fatalf("encounter = %+v", enc)
	}
}

func TestCharacterMapsDoNotAliasCallerInput(t *testing.T) {
	s := newService(t)
	in := goblin()
	r := mustRoster(t, s, "u1", in)
	in.Health["hp"] = 99
	if r.Characters[0].Health["hp"] != 7 {
		t.Fatalf("roster health = %v, want 7: caller's map is aliased", r.Characters[0].Health)
	}
}

func TestOtherOwnersRosterWritesAreForbidden(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1", goblin())
	id := r.Characters[0].ID
	tests := []struct {
		name string
		call func() error
	}{
		{"rename", func() error { _, err := s.Rename(ctx, "u2", "u2", r.ID, 1, "x", false); return err }},
		{"delete", func() error { return s.Delete(ctx, "u2", "u2", r.ID, 1) }},
		{"add character", func() error { _, err := s.AddCharacter(ctx, "u2", "u2", r.ID, 1, goblin()); return err }},
		{"update character", func() error { _, err := s.UpdateCharacter(ctx, "u2", "u2", r.ID, 1, id, goblin()); return err }},
		{"remove character", func() error { _, err := s.RemoveCharacter(ctx, "u2", "u2", r.ID, 1, id); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, data.ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestMissingRosterWritesAreNotFound(t *testing.T) {
	s := newService(t)
	if _, err := s.AddCharacter(ctx, "u1", "u1", "nope", 1, goblin()); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateEncounterFromOthersRosterIsNotFound(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1", goblin())
	if _, err := s.CreateEncounterFromRoster(ctx, "u2", "u2", r.ID, baseEncounter()); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSaveAsRosterSkipsRemovedAndLeavesEncounterAlone(t *testing.T) {
	s := newService(t)
	enc := baseEncounter()
	enc.Combatants = []data.Combatant{
		{ID: "c1", Name: "Goblin", Kind: data.KindAdversary, Health: map[string]int{"hp": 3}},
		{ID: "c2", Name: "Gone", Kind: data.KindAdversary, Removed: true},
	}
	if err := s.encounters.Create(ctx, "u1", "u1", &enc); err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveAsRoster(ctx, "u1", "u1", enc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.encounters.Get(ctx, "u1", enc.ID)
	if saved.Name != "Ambush" || len(saved.Characters) != 1 || saved.Characters[0].Health["hp"] != 3 {
		t.Fatalf("saved = %+v", saved)
	}
	if after.Revision != enc.Revision || len(after.Combatants) != 2 {
		t.Fatalf("encounter changed: %+v", after)
	}
}

func TestSaveAsRosterOfOthersEncounterIsNotFound(t *testing.T) {
	s := newService(t)
	r := mustRoster(t, s, "u1", goblin())
	enc, _ := s.CreateEncounterFromRoster(ctx, "u1", "u1", r.ID, baseEncounter())
	if _, err := s.SaveAsRoster(ctx, "u2", "u2", enc.ID, "x"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
