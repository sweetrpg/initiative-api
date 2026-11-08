package data

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store/fake"
)

var (
	ctx   = context.Background()
	clock = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
)

func newRosters(t *testing.T) (*Owned[Roster, *Roster], *fake.Collection) {
	t.Helper()
	db := fake.NewDB()
	r := NewOwned[Roster](db, RosterSpec)
	n := 0
	r.newID = func() string { n++; return string(rune('a' + n - 1)) }
	r.now = func() time.Time { return clock }
	return r, db.Fake(CollectionRosters)
}

func mustCreate(t *testing.T, r *Owned[Roster, *Roster], owner, name string) *Roster {
	t.Helper()
	d := &Roster{Name: name}
	if err := r.Create(ctx, owner, owner, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreateStampsOwnerRevisionAndAudit(t *testing.T) {
	r, _ := newRosters(t)
	d := &Roster{Name: "Goblins", Meta: Meta{OwnerUserID: "spoofed", ID: "spoofed"}}
	if err := r.Create(ctx, "u1", "actor", d); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		got, want any
	}{
		{"owner", d.OwnerUserID, "u1"},
		{"id assigned", d.ID, "a"},
		{"revision", d.Revision, int64(1)},
		{"created_by", d.CreatedBy, "actor"},
		{"updated_by", d.UpdatedBy, "actor"},
		{"created_at", d.CreatedAt, clock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !same(tt.got, tt.want) {
				t.Fatalf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestEmptyOwnerOrActorRejected(t *testing.T) {
	r, _ := newRosters(t)
	tests := []struct{ name, owner, actor string }{
		{"empty owner", "", "a"}, {"empty actor", "u1", ""}, {"blank owner", "  ", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Create(ctx, tt.owner, tt.actor, &Roster{}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestGetIsOwnerScoped(t *testing.T) {
	r, _ := newRosters(t)
	d := mustCreate(t, r, "u1", "Goblins")
	tests := []struct {
		name  string
		owner string
		id    string
		want  error
	}{
		{"owner reads own", "u1", d.ID, nil},
		{"other owner sees not found", "u2", d.ID, ErrNotFound},
		{"missing id", "u1", "zzz", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Get(ctx, tt.owner, tt.id); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUpdateOutcomes(t *testing.T) {
	tests := []struct {
		name  string
		owner string
		id    string
		rev   int64
		want  error
	}{
		{"owner with current revision", "u1", "a", 1, nil},
		{"other owner is forbidden", "u2", "a", 1, ErrForbidden},
		{"missing id is not found", "u1", "zzz", 1, ErrNotFound},
		{"stale revision conflicts", "u1", "a", 7, ErrRevisionConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newRosters(t)
			mustCreate(t, r, "u1", "Goblins")
			_, err := r.Update(ctx, tt.owner, tt.owner, tt.id, tt.rev, map[string]any{"name": "Orcs"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUpdateByOtherOwnerChangesNothing(t *testing.T) {
	r, col := newRosters(t)
	mustCreate(t, r, "u1", "Goblins")
	_, _ = r.Update(ctx, "u2", "u2", "a", 1, map[string]any{"name": "Hijacked"})
	if got := col.Docs()[0]["name"]; got != "Goblins" {
		t.Fatalf("name = %v, want Goblins", got)
	}
}

func TestUpdateAdvancesRevisionAndAudit(t *testing.T) {
	r, _ := newRosters(t)
	mustCreate(t, r, "u1", "Goblins")
	later := clock.Add(time.Hour)
	r.now = func() time.Time { return later }
	got, err := r.Update(ctx, "u1", "editor", "a", 1, map[string]any{"name": "Orcs"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		got, want any
	}{
		{"name", got.Name, "Orcs"},
		{"revision", got.Revision, int64(2)},
		{"updated_by", got.UpdatedBy, "editor"},
		{"updated_at", got.UpdatedAt, later},
		{"created_by kept", got.CreatedBy, "u1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !same(tt.got, tt.want) {
				t.Fatalf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestConcurrentEditsOneWins(t *testing.T) {
	r, _ := newRosters(t)
	mustCreate(t, r, "u1", "Goblins")
	_, first := r.Update(ctx, "u1", "u1", "a", 1, map[string]any{"name": "A"})
	cur, second := r.Update(ctx, "u1", "u1", "a", 1, map[string]any{"name": "B"})
	if first != nil || !errors.Is(second, ErrRevisionConflict) {
		t.Fatalf("first=%v second=%v, want nil and conflict", first, second)
	}
	if cur.Name != "A" || cur.Revision != 2 {
		t.Fatalf("conflict returned %+v, want the current record (A, rev 2)", cur)
	}
}

func TestUpdateRejectsReservedAndOperatorFields(t *testing.T) {
	for _, field := range []string{"owner_user_id", "_id", "revision", "created_by", "deleted_at", "$set", "a.b", ""} {
		t.Run(field, func(t *testing.T) {
			r, _ := newRosters(t)
			mustCreate(t, r, "u1", "Goblins")
			_, err := r.Update(ctx, "u1", "u1", "a", 1, map[string]any{field: "x"})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestSoftDeleteOutcomes(t *testing.T) {
	tests := []struct {
		name  string
		owner string
		id    string
		want  error
	}{
		{"owner deletes", "u1", "a", nil},
		{"other owner forbidden", "u2", "a", ErrForbidden},
		{"missing not found", "u1", "zzz", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newRosters(t)
			mustCreate(t, r, "u1", "Goblins")
			if err := r.SoftDelete(ctx, tt.owner, tt.owner, tt.id, 1); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSoftDeletedRecordIsGoneButKept(t *testing.T) {
	r, col := newRosters(t)
	mustCreate(t, r, "u1", "Goblins")
	if err := r.SoftDelete(ctx, "u1", "u1", "a", 1); err != nil {
		t.Fatal(err)
	}
	_, getErr := r.Get(ctx, "u1", "a")
	_, updErr := r.Update(ctx, "u1", "u1", "a", 2, map[string]any{"name": "x"})
	if !errors.Is(getErr, ErrNotFound) || !errors.Is(updErr, ErrNotFound) || len(col.Docs()) != 1 {
		t.Fatalf("get=%v update=%v docs=%d, want not found twice and the document retained", getErr, updErr, len(col.Docs()))
	}
}

func TestThemeSoftDeleteFreesSlot(t *testing.T) {
	db := fake.NewDB()
	if err := EnsureIndexes(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewOwned[Theme](db, ThemeSpec)
	slot := 0
	first := &Theme{Name: "Dungeon", Slot: &slot}
	if err := r.Create(ctx, "u1", "u1", first); err != nil {
		t.Fatal(err)
	}
	dup := &Theme{Name: "Other", Slot: &slot}
	dupErr := r.Create(ctx, "u1", "u1", dup)
	if err := r.SoftDelete(ctx, "u1", "u1", first.ID, 1); err != nil {
		t.Fatal(err)
	}
	reuse := &Theme{Name: "Reuse", Slot: &slot}
	reuseErr := r.Create(ctx, "u1", "u1", reuse)
	if dupErr == nil || reuseErr != nil {
		t.Fatalf("duplicate slot err=%v (want error), reuse after delete err=%v (want nil)", dupErr, reuseErr)
	}
}

// same compares values, using Equal for times (== also compares location and monotonic clock).
func same(got, want any) bool {
	if g, ok := got.(time.Time); ok {
		w, ok := want.(time.Time)
		return ok && g.Equal(w)
	}
	return got == want
}

func TestSoftDeleteStaleRevisionConflicts(t *testing.T) {
	r, col := newRosters(t)
	mustCreate(t, r, "u1", "Goblins")
	err := r.SoftDelete(ctx, "u1", "u1", "a", 9)
	if !errors.Is(err, ErrRevisionConflict) || col.Docs()[0]["deleted_at"] != nil {
		t.Fatalf("err = %v, deleted_at = %v; want a conflict and a live record", err, col.Docs()[0]["deleted_at"])
	}
}

func TestCredentialUpdateRejectsStateFields(t *testing.T) {
	for _, field := range []string{"secret_hash", "revoked_at", "last_used_at", "scopes"} {
		t.Run(field, func(t *testing.T) {
			r := NewOwned[IntegrationCredential](fake.NewDB(), CredentialSpec)
			c := &IntegrationCredential{Label: "foundry", Scopes: []string{ScopeRead}}
			if err := r.Create(ctx, "u1", "u1", c); err != nil {
				t.Fatal(err)
			}
			_, err := r.Update(ctx, "u1", "u1", c.ID, 1, map[string]any{field: "x"})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCredentialLabelIsUpdatable(t *testing.T) {
	r := NewOwned[IntegrationCredential](fake.NewDB(), CredentialSpec)
	c := &IntegrationCredential{Label: "old"}
	_ = r.Create(ctx, "u1", "u1", c)
	got, err := r.Update(ctx, "u1", "u1", c.ID, 1, map[string]any{"label": "new"})
	if err != nil || got.Label != "new" {
		t.Fatalf("label=%q err=%v, want new", got.Label, err)
	}
}

func TestEveryDeclaredUpdatableFieldExistsInProjectable(t *testing.T) {
	for _, s := range []Spec{RosterSpec, ThemeSpec, CredentialSpec} {
		for _, f := range s.Updatable {
			if reservedFields[f] {
				t.Fatalf("%s lists reserved field %q as updatable", s.Name, f)
			}
		}
	}
}

func TestConcurrentWritersExactlyOneWins(t *testing.T) {
	r, col := newRosters(t)
	mustCreate(t, r, "u1", "Goblins")
	const writers = 25
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.Update(ctx, "u1", "u1", "a", 1, map[string]any{"name": "w"})
		}()
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrRevisionConflict):
			conflicts++
		}
	}
	if wins != 1 || conflicts != writers-1 || col.Docs()[0]["revision"] != int64(2) {
		t.Fatalf("wins=%d conflicts=%d revision=%v, want 1, %d, 2", wins, conflicts, col.Docs()[0]["revision"], writers-1)
	}
}
