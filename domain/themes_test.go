package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store/fake"
)

type stubChecker struct {
	limit int
	flag  bool
	err   error
}

func (c stubChecker) Check(_ context.Context, key string, _ entitlements.Policy) (entitlements.Result, error) {
	return entitlements.Result{Key: key, Type: "limit", Limit: c.limit, Flag: c.flag}, c.err
}

func newThemes(t *testing.T) *Themes {
	t.Helper()
	s, err := NewThemes(fake.NewDB())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dungeon(name string) ThemeInput {
	return ThemeInput{
		Name: name, Style: "dark", MainColor: "#FFFFFF", Accent1Color: "#AA0000", Accent2Color: "#00AA00",
		Background1Color: "#000000", Background2Color: "#111111",
		PrimaryFontName: "Cinzel", SecondaryFontName: "Lato",
	}
}

func TestThemeCreateAssignsLowestFreeSlot(t *testing.T) {
	s, chk := newThemes(t), stubChecker{limit: 3}
	a, err := s.Create(ctx, "u1", "u1", dungeon("A"), chk)
	if err != nil || a.Slot == nil || *a.Slot != 0 {
		t.Fatalf("first theme = %+v, %v; want slot 0", a, err)
	}
	b, _ := s.Create(ctx, "u1", "u1", dungeon("B"), chk)
	if err := s.Delete(ctx, "u1", "u1", a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	c, err := s.Create(ctx, "u1", "u1", dungeon("C"), chk)
	if err != nil || *c.Slot != 0 || *b.Slot != 1 {
		t.Fatalf("after delete: c=%+v b=%+v err=%v; want freed slot 0 reused", c.Slot, b.Slot, err)
	}
}

func TestThemeCreateBeyondLimitIsTypedEntitlementError(t *testing.T) {
	s, chk := newThemes(t), stubChecker{limit: 1}
	if _, err := s.Create(ctx, "u1", "u1", dungeon("A"), chk); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create(ctx, "u1", "u1", dungeon("B"), chk)
	var ee *EntitlementError
	if !errors.As(err, &ee) || ee.Key != KeyThemeSlots || ee.Limit != 1 {
		t.Fatalf("err = %v, want EntitlementError{%s, 1}", err, KeyThemeSlots)
	}
}

func TestThemeLimitIsPerUser(t *testing.T) {
	s, chk := newThemes(t), stubChecker{limit: 1}
	_, _ = s.Create(ctx, "u1", "u1", dungeon("A"), chk)
	if _, err := s.Create(ctx, "u2", "u2", dungeon("A"), chk); err != nil {
		t.Fatalf("another user's theme counted against u2: %v", err)
	}
}

func TestThemeCreateFailsClosedWhenEntitlementsDown(t *testing.T) {
	s := newThemes(t)
	_, err := s.Create(ctx, "u1", "u1", dungeon("A"), stubChecker{err: entitlements.ErrUnavailable})
	if !errors.Is(err, entitlements.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestThemeValidation(t *testing.T) {
	bad := map[string]func(*ThemeInput){
		"empty name":     func(i *ThemeInput) { i.Name = " " },
		"bad color":      func(i *ThemeInput) { i.MainColor = "red" },
		"empty font":     func(i *ThemeInput) { i.PrimaryFontName = "" },
		"http image":     func(i *ThemeInput) { i.BackgroundImage = "http://x/y.png" },
		"javascript img": func(i *ThemeInput) { i.BackgroundImage = "javascript:alert(1)" },
	}
	for name, mut := range bad {
		in := dungeon("A")
		mut(&in)
		if _, err := newThemes(t).Create(ctx, "u1", "u1", in, stubChecker{limit: 5}); !errors.Is(err, data.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestPackagedThemesAreReadOnly(t *testing.T) {
	s := newThemes(t)
	pk := s.Packaged()
	if len(pk) == 0 {
		t.Fatal("no packaged themes")
	}
	got, err := s.Get(ctx, "u1", pk[0].ID)
	if err != nil || got.ID != pk[0].ID {
		t.Fatalf("get packaged = %+v, %v", got, err)
	}
	name := "x"
	if _, err := s.Update(ctx, "u1", "u1", pk[0].ID, 1, ThemePatch{Name: &name}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("update err = %v, want ErrReadOnly", err)
	}
	if err := s.Delete(ctx, "u1", "u1", pk[0].ID, 1); !errors.Is(err, ErrReadOnly) {
		t.Errorf("delete err = %v, want ErrReadOnly", err)
	}
}

func TestThemeUpdateAndOwnership(t *testing.T) {
	s, chk := newThemes(t), stubChecker{limit: 2}
	th, _ := s.Create(ctx, "u1", "u1", dungeon("A"), chk)
	name := "Renamed"
	got, err := s.Update(ctx, "u1", "u1", th.ID, th.Revision, ThemePatch{Name: &name})
	if err != nil || got.Name != "Renamed" || got.Revision != th.Revision+1 {
		t.Fatalf("update = %+v, %v", got, err)
	}
	if _, err := s.Update(ctx, "u2", "u2", th.ID, got.Revision, ThemePatch{Name: &name}); !errors.Is(err, data.ErrForbidden) {
		t.Errorf("other owner err = %v, want ErrForbidden", err)
	}
	if _, err := s.Update(ctx, "u1", "u1", th.ID, th.Revision, ThemePatch{Name: &name}); !errors.Is(err, data.ErrRevisionConflict) {
		t.Errorf("stale err = %v, want ErrRevisionConflict", err)
	}
	if _, err := s.Get(ctx, "u2", th.ID); !errors.Is(err, data.ErrNotFound) {
		t.Errorf("other owner read err = %v, want ErrNotFound", err)
	}
}
