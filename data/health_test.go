package data

import (
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func TestCheckHealthBoundsAndReferences(t *testing.T) {
	sys := GameSystem{Fields: []TrackerField{
		{Key: "hp", Kind: FieldCounter, Min: ip(0), MaxRef: "max_hp"},
		{Key: "max_hp", Kind: FieldCounter, Min: ip(0), Max: ip(20)},
		{Key: "dead", Kind: FieldFlag},
		{Key: "successes", Kind: FieldTrack, Min: ip(0), Max: ip(3)},
	}}
	tests := []struct {
		name  string
		h     map[string]int
		field string // "" means valid
	}{
		{"valid", map[string]int{"hp": 5, "max_hp": 10, "dead": 0, "successes": 3}, ""},
		{"unknown key", map[string]int{"mana": 1}, "mana"},
		{"below min", map[string]int{"hp": -5}, "hp"},
		{"above literal max", map[string]int{"max_hp": 21}, "max_hp"},
		{"track above max", map[string]int{"successes": 4}, "successes"},
		{"track below min", map[string]int{"successes": -1}, "successes"},
		{"above max_ref", map[string]int{"hp": 11, "max_hp": 10}, "hp"},
		{"max_ref absent is not checked", map[string]int{"hp": 50}, ""},
		{"flag set", map[string]int{"dead": 1}, ""},
		{"flag above one even without bounds", map[string]int{"dead": 2}, "dead"},
		{"flag negative even without bounds", map[string]int{"dead": -1}, "dead"},
		{"first sorted bad field is reported", map[string]int{"zz": 1, "aa": 1}, "aa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckHealth(sys, tt.h)
			var he *HealthError
			switch {
			case tt.field == "" && err != nil:
				t.Fatalf("err = %v, want valid", err)
			case tt.field != "" && (!errors.As(err, &he) || he.Field != tt.field || !errors.Is(err, ErrInvalid)):
				t.Fatalf("err = %v, want HealthError on %q wrapping ErrInvalid", err, tt.field)
			}
		})
	}
}

func TestValidateSystemKeepsFlagsBinary(t *testing.T) {
	flag := func(f TrackerField) GameSystem {
		f.Key, f.Kind = "dead", FieldFlag
		return GameSystem{ID: "s", Name: "S", Family: "f", Fields: []TrackerField{f}}
	}
	tests := []struct {
		name string
		f    TrackerField
		ok   bool
	}{
		{"no bounds", TrackerField{}, true},
		{"zero to one", TrackerField{Min: ip(0), Max: ip(1)}, true},
		{"default one", TrackerField{Def: 1}, true},
		{"default two", TrackerField{Def: 2}, false},
		{"max five", TrackerField{Max: ip(5)}, false},
		{"min negative", TrackerField{Min: ip(-1)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSystem(flag(tt.f)); (err == nil) != tt.ok {
				t.Fatalf("err = %v, ok want %v", err, tt.ok)
			}
		})
	}
}

func seedSystems(t *testing.T, db *fake.DB, systems ...GameSystem) *SystemRepo {
	t.Helper()
	repo := NewSystemRepo(db)
	for _, s := range systems {
		if _, err := repo.Upsert(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func hpSystem(id string, max int) GameSystem {
	return GameSystem{ID: id, Name: id, Family: "f", Fields: []TrackerField{
		{Key: "hp", Label: "HP", Kind: FieldCounter, Min: ip(0), Max: ip(max)}}}
}

func TestGetOrDefaultFallsBackToNoSystem(t *testing.T) {
	db := fake.NewDB()
	repo := seedSystems(t, db, hpSystem(DefaultSystemID, 5), hpSystem("known", 9))
	for id, want := range map[string]string{"known": "known", "vanished": DefaultSystemID} {
		got, err := repo.GetOrDefault(ctx, id)
		if err != nil || got.ID != want {
			t.Fatalf("GetOrDefault(%q) = %q, %v, want %q", id, got.ID, err, want)
		}
	}
}

func TestGetOrDefaultWithoutDefaultIsNotFound(t *testing.T) {
	repo := seedSystems(t, fake.NewDB())
	if _, err := repo.GetOrDefault(ctx, "vanished"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func healthEncounter(t *testing.T, db *fake.DB, sysID string, cs ...Combatant) (*Encounters, *Encounter) {
	t.Helper()
	r := NewEncounters(db)
	e := &Encounter{Name: "x", GameSystemID: sysID, Ordering: OrderHighToLow, TieBreaker: TieRandom, Combatants: cs}
	if err := r.Create(ctx, "u1", "u1", e); err != nil {
		t.Fatal(err)
	}
	return r, e
}

func withHP(id string, hp int) Combatant {
	c := cb(id)
	c.Health = map[string]int{"hp": hp}
	return c
}

func TestEncounterWithUndefinedSystemUsesTheDefaultDefinition(t *testing.T) {
	db := fake.NewDB()
	seedSystems(t, db, hpSystem(DefaultSystemID, 5))
	r, e := healthEncounter(t, db, "vanished", withHP("c1", 3))
	_, err := r.SetState(ctx, "u1", "u1", e.ID, 1, EncounterState{Status: StatusDraft, Combatants: []Combatant{withHP("c1", 9)}})
	var he *HealthError
	if !errors.As(err, &he) || he.Field != "hp" {
		t.Fatalf("err = %v, want HealthError on hp from the default definition (max 5)", err)
	}
}

func TestStaleStoredHealthDoesNotBlockUnrelatedEdits(t *testing.T) {
	db := fake.NewDB()
	repo := seedSystems(t, db, hpSystem("s", 10))
	r, e := healthEncounter(t, db, "s", withHP("c1", 8), withHP("c2", 2))
	seedSystems(t, db, hpSystem("s", 5)) // the definition tightens: c1's stored 8 is now out of range
	if _, err := repo.Get(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	renamed := withHP("c2", 2)
	renamed.Name = "Renamed"
	got, err := r.SetState(ctx, "u1", "u1", e.ID, 1, EncounterState{Status: StatusDraft, Combatants: []Combatant{withHP("c1", 8), renamed}})
	if err != nil || got.Combatants[1].Name != "Renamed" {
		t.Fatalf("unrelated edit blocked by stale value: %v", err)
	}
	_, err = r.SetState(ctx, "u1", "u1", e.ID, 2, EncounterState{Status: StatusDraft, Combatants: []Combatant{withHP("c1", 7), renamed}})
	var he *HealthError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want HealthError when c1's health itself changes", err)
	}
}

func TestSwitchingSystemRechecksEveryCombatant(t *testing.T) {
	db := fake.NewDB()
	seedSystems(t, db, hpSystem("big", 10), hpSystem("small", 5))
	r, e := healthEncounter(t, db, "big", withHP("c1", 8))
	_, err := r.Write(ctx, "u1", "u1", e.ID, 1, map[string]any{"game_system_id": "small"},
		EncounterState{Status: StatusDraft, Combatants: []Combatant{withHP("c1", 8)}})
	var he *HealthError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want HealthError: 8 exceeds the new system's max 5", err)
	}
}
