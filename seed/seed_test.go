package seed

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store/fake"
)

func keys(s data.GameSystem) string {
	k := make([]string, len(s.Fields))
	for i, f := range s.Fields {
		k[i] = f.Key
	}
	sort.Strings(k)
	return fmt.Sprint(k)
}

func byID(t *testing.T) map[string]data.GameSystem {
	t.Helper()
	defs, err := Definitions()
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]data.GameSystem{}
	for _, d := range defs {
		m[d.ID] = d
	}
	return m
}

func TestFamilyFieldSets(t *testing.T) {
	tests := []struct{ system, want string }{
		{"none", "[health]"},
		{"d20", "[current dead maximum temporary]"},
		{"dnd5", "[current failureCount maximum successCount temporary]"},
		{"cypher", "[intellect intellectMaximum might mightMaximum speed speedMaximum]"},
		{"theonering", "[endurance enduranceRating fatigue hope hopeRating miserable shadow weary wounded]"},
		{"shadowrun5", "[value1 value1Maximum value2 value2Maximum]"},
	}
	defs := byID(t)
	for _, tt := range tests {
		t.Run(tt.system, func(t *testing.T) {
			if got := keys(defs[tt.system]); got != tt.want {
				t.Fatalf("fields = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSystemsSharingAFamilyKeepOwnNames(t *testing.T) {
	d := byID(t)
	if keys(d["shadowrun5"]) != keys(d["genesys"]) || d["shadowrun5"].Name == d["genesys"].Name {
		t.Fatal("shadowrun5 and genesys should share twovalue fields but differ in name")
	}
}

func TestDnd5Bounds(t *testing.T) {
	for _, f := range byID(t)["dnd5"].Fields {
		if (f.Key == "successCount" || f.Key == "failureCount") && (*f.Min != 0 || *f.Max != 3) {
			t.Fatalf("%s bounds = %d..%d, want 0..3", f.Key, *f.Min, *f.Max)
		}
	}
}

func TestOnlyNoSystemIsUngated(t *testing.T) {
	for id, d := range byID(t) {
		if d.Gated == (id == "none") {
			t.Fatalf("%s gated=%v", id, d.Gated)
		}
	}
}

func TestLoadIsIdempotent(t *testing.T) {
	db := fake.NewDB()
	if err := data.EnsureIndexes(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	repo := data.NewSystemRepo(db)
	first, err1 := Load(context.Background(), repo)
	before := db.Fake(data.CollectionGameSystems).Docs()
	second, err2 := Load(context.Background(), repo)
	after := db.Fake(data.CollectionGameSystems).Docs()
	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v, %v", err1, err2)
	}
	if first.Created != len(before) || second.Created != 0 || second.Unchanged != len(before) || fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("first=%+v second=%+v docs before=%d after=%d, want a no-op second run", first, second, len(before), len(after))
	}
}

func TestResolveFoundryAlias(t *testing.T) {
	db := fake.NewDB()
	repo := data.NewSystemRepo(db)
	if _, err := Load(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(context.Background(), "dnd5e")
	if err != nil || got.ID != "dnd5" {
		t.Fatalf("got %q err=%v, want dnd5", got.ID, err)
	}
}

func TestExpandRejectsIdentifierCollisions(t *testing.T) {
	fam := map[string][]data.TrackerField{"f": {{Key: "k", Label: "l", Kind: data.FieldCounter, Def: 1}}}
	tests := []struct {
		name    string
		systems []systemFile
	}{
		{"alias equals another system's id", []systemFile{
			{ID: "alpha", Name: "A", Family: "f"}, {ID: "beta", Name: "B", Family: "f", Aliases: []string{"alpha"}}}},
		{"alias equals a later system's id", []systemFile{
			{ID: "beta", Name: "B", Family: "f", Aliases: []string{"alpha"}}, {ID: "alpha", Name: "A", Family: "f"}}},
		{"alias shared by two systems", []systemFile{
			{ID: "alpha", Name: "A", Family: "f", Aliases: []string{"x"}}, {ID: "beta", Name: "B", Family: "f", Aliases: []string{"x"}}}},
		{"duplicate id", []systemFile{{ID: "alpha", Name: "A", Family: "f"}, {ID: "alpha", Name: "A2", Family: "f"}}},
		{"unknown family", []systemFile{{ID: "alpha", Name: "A", Family: "nope"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := expand(fam, tt.systems); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestExpandAcceptsDistinctIdentifiers(t *testing.T) {
	fam := map[string][]data.TrackerField{"f": {{Key: "k", Label: "l", Kind: data.FieldCounter, Def: 1}}}
	_, err := expand(fam, []systemFile{{ID: "alpha", Name: "A", Family: "f", Aliases: []string{"a1"}}, {ID: "beta", Name: "B", Family: "f"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
