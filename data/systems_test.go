package data

import (
	"errors"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

func ip(n int) *int { return &n }

func okSystem() GameSystem {
	return GameSystem{ID: "sys", Name: "Sys", Family: "f", Aliases: []string{"sys5e"}, Fields: []TrackerField{
		{Key: "current", Label: "l", Kind: FieldCounter, Def: 1, Min: ip(0), MaxRef: "maximum"},
		{Key: "maximum", Label: "l", Kind: FieldCounter, Def: 1, Min: ip(1)},
		{Key: "count", Label: "l", Kind: FieldTrack, Min: ip(0), Max: ip(3)},
	}}
}

func TestValidateSystem(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GameSystem)
		ok     bool
	}{
		{"valid", func(*GameSystem) {}, true},
		{"uppercase id", func(s *GameSystem) { s.ID = "Sys" }, false},
		{"no fields", func(s *GameSystem) { s.Fields = nil }, false},
		{"bad alias", func(s *GameSystem) { s.Aliases = []string{"Has Space"} }, false},
		{"duplicate key", func(s *GameSystem) { s.Fields[2].Key = "maximum" }, false},
		{"unknown kind", func(s *GameSystem) { s.Fields[0].Kind = "dial" }, false},
		{"dangling max_ref", func(s *GameSystem) { s.Fields[0].MaxRef = "nope" }, false},
		{"self max_ref", func(s *GameSystem) { s.Fields[0].MaxRef = "current" }, false},
		{"max and max_ref", func(s *GameSystem) { s.Fields[0].Max = ip(5) }, false},
		{"min above max", func(s *GameSystem) { s.Fields[2].Min = ip(4) }, false},
		{"default above max", func(s *GameSystem) { s.Fields[2].Def = 4 }, false},
		{"default above ref default", func(s *GameSystem) { s.Fields[0].Def = 9 }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := okSystem()
			tt.mutate(&s)
			err := ValidateSystem(s)
			if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func newSystems(t *testing.T) (*SystemRepo, *fake.Collection) {
	t.Helper()
	db := fake.NewDB()
	_ = EnsureIndexes(ctx, db)
	r := NewSystemRepo(db)
	r.now = func() time.Time { return clock }
	return r, db.Fake(CollectionGameSystems)
}

func TestUpsertOutcomes(t *testing.T) {
	r, _ := newSystems(t)
	first, _ := r.Upsert(ctx, okSystem())
	again, _ := r.Upsert(ctx, okSystem())
	changed := okSystem()
	changed.Name = "Renamed"
	third, _ := r.Upsert(ctx, changed)
	if first != Created || again != Unchanged || third != Updated {
		t.Fatalf("outcomes = %s, %s, %s; want created, unchanged, updated", first, again, third)
	}
}

func TestUpsertKeepsIdentityAndStampsSystemActor(t *testing.T) {
	r, col := newSystems(t)
	_, _ = r.Upsert(ctx, okSystem())
	_, _ = r.Upsert(ctx, okSystem())
	d := col.Docs()
	if len(d) != 1 || d[0]["_id"] != "sys" || d[0]["created_by"] != "system" || d[0]["revision"] != int64(1) {
		t.Fatalf("docs = %v, want one 'sys' doc by system at revision 1", d)
	}
}

func TestUpdateBumpsRevision(t *testing.T) {
	r, _ := newSystems(t)
	_, _ = r.Upsert(ctx, okSystem())
	changed := okSystem()
	changed.Gated = true
	_, _ = r.Upsert(ctx, changed)
	got, _ := r.Get(ctx, "sys")
	if got.Revision != 2 || !got.Gated {
		t.Fatalf("got revision %d gated %v, want 2 and true", got.Revision, got.Gated)
	}
}

func TestAliasUniqueAcrossSystems(t *testing.T) {
	r, _ := newSystems(t)
	_, _ = r.Upsert(ctx, okSystem())
	other := okSystem()
	other.ID = "other"
	if _, err := r.Upsert(ctx, other); err == nil {
		t.Fatal("want an alias conflict error, got nil")
	}
}

func TestGetByIDOrAlias(t *testing.T) {
	r, _ := newSystems(t)
	_, _ = r.Upsert(ctx, okSystem())
	tests := []struct {
		name, key string
		want      error
	}{
		{"by id", "sys", nil}, {"by alias", "sys5e", nil}, {"unknown", "nope", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Get(ctx, tt.key); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestListFiltersGatedInTheQuery(t *testing.T) {
	r, col := newSystems(t)
	free := okSystem()
	free.ID, free.Aliases = "free", []string{}
	_, _ = r.Upsert(ctx, free)
	gated := okSystem()
	gated.Gated = true
	_, _ = r.Upsert(ctx, gated)
	p, err := r.List(ctx, Query{Filters: []Filter{{"gated", OpEq, []any{false}}}})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != "free" || len(col.FindCalls) != 1 {
		t.Fatalf("items=%v err=%v, want only the ungated system", p.Items, err)
	}
}
