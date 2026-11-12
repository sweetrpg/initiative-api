package domain

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/store"
)

// KeyThemeSlots is the entitlement limit on custom theme slots (docs/entitlements-check-contract.md).
const KeyThemeSlots = "initiative.theme_slots"

// PackagedPrefix starts the ID of every packaged theme. Custom theme IDs never carry it.
const PackagedPrefix = "pkg-"

const slotRetries = 3

var (
	// ErrReadOnly means the caller tried to change or delete a packaged theme.
	ErrReadOnly = errors.New("domain: packaged theme is read-only")

	hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}([0-9A-Fa-f]{2})?$`)
)

//go:embed packaged_themes.json
var packagedJSON []byte

// EntitlementError is the typed denial for a stored-item limit the caller has reached.
type EntitlementError struct {
	Key   string
	Limit int
}

func (e *EntitlementError) Error() string {
	return fmt.Sprintf("domain: entitlement limit reached for %s (%d)", e.Key, e.Limit)
}

// Checker answers entitlement checks for the calling user; the server binds the user and token.
type Checker interface {
	Check(ctx context.Context, key string, policy entitlements.Policy) (entitlements.Result, error)
}

// ThemeInput is the full field set of a custom theme.
type ThemeInput struct {
	Name, Style, MainColor, Accent1Color, Accent2Color, Background1Color, Background2Color string
	PrimaryFontName, SecondaryFontName, BackgroundImage                                    string
}

// ThemePatch changes the fields that are non-nil.
type ThemePatch struct {
	Name, Style, MainColor, Accent1Color, Accent2Color, Background1Color, Background2Color *string
	PrimaryFontName, SecondaryFontName, BackgroundImage                                    *string
}

// Themes implements custom theme rules: slot enforcement and read-only packaged themes.
type Themes struct {
	themes   *data.Owned[data.Theme, *data.Theme]
	packaged []data.Theme
}

// NewThemes builds the theme service on db.
func NewThemes(db store.Database) (*Themes, error) {
	var pk []data.Theme
	if err := json.Unmarshal(packagedJSON, &pk); err != nil {
		return nil, fmt.Errorf("packaged themes: %w", err)
	}
	return &Themes{themes: data.NewOwned[data.Theme](db, data.ThemeSpec), packaged: pk}, nil
}

// Packaged returns the read-only packaged themes.
func (s *Themes) Packaged() []data.Theme { return append([]data.Theme(nil), s.packaged...) }

func isPackaged(id string) bool { return strings.HasPrefix(id, PackagedPrefix) }

// Get returns a packaged theme by ID or owner's custom theme.
func (s *Themes) Get(ctx context.Context, owner, id string) (data.Theme, error) {
	if isPackaged(id) {
		for _, t := range s.packaged {
			if t.ID == id {
				return t, nil
			}
		}
		return data.Theme{}, data.ErrNotFound
	}
	return s.themes.Get(ctx, owner, id)
}

// List returns a page of owner's custom themes in slot order.
func (s *Themes) List(ctx context.Context, owner string, q data.Query) (data.Page[data.Theme], error) {
	if len(q.Sort) == 0 {
		q.Sort = []data.SortKey{{Field: "slot"}}
	}
	return s.themes.List(ctx, owner, q)
}

// Create stores a custom theme in the lowest free slot below the entitled limit. At or over the
// limit it returns *EntitlementError; if entitlements-api is unreachable, entitlements.ErrUnavailable.
func (s *Themes) Create(ctx context.Context, owner, actor string, in ThemeInput, chk Checker) (data.Theme, error) {
	t, err := buildTheme(in)
	if err != nil {
		return data.Theme{}, err
	}
	res, err := chk.Check(ctx, KeyThemeSlots, entitlements.FailClosed)
	if err != nil {
		return data.Theme{}, err
	}
	// Inactive themes count toward the total owned, so a user over a reduced limit cannot create.
	owned, err := s.themes.Count(ctx, owner, nil)
	if err != nil {
		return data.Theme{}, err
	}
	if owned >= int64(res.Limit) {
		return data.Theme{}, &EntitlementError{Key: KeyThemeSlots, Limit: res.Limit}
	}
	for range slotRetries {
		slot, err := s.freeSlot(ctx, owner, res.Limit)
		if err != nil {
			return data.Theme{}, err
		}
		t.Slot = &slot
		err = s.themes.Create(ctx, owner, actor, &t)
		if !errors.Is(err, data.ErrConflict) {
			return t, err
		}
	}
	return data.Theme{}, data.ErrConflict
}

// freeSlot returns the lowest unused slot in [0, limit), or *EntitlementError when all are used.
func (s *Themes) freeSlot(ctx context.Context, owner string, limit int) (int, error) {
	page, err := s.themes.List(ctx, owner, data.Query{
		Sort: []data.SortKey{{Field: "slot"}}, Fields: []string{"slot"}, Limit: limit,
	})
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, t := range page.Items {
		if t.Slot != nil {
			used[*t.Slot] = true
		}
	}
	for i := range limit {
		if !used[i] {
			return i, nil
		}
	}
	return 0, &EntitlementError{Key: KeyThemeSlots, Limit: limit}
}

// Update changes owner's custom theme, compare-and-set on rev. Packaged themes return ErrReadOnly.
func (s *Themes) Update(ctx context.Context, owner, actor, id string, rev int64, p ThemePatch) (data.Theme, error) {
	if isPackaged(id) {
		return data.Theme{}, ErrReadOnly
	}
	set, err := patchSet(p)
	if err != nil {
		return data.Theme{}, err
	}
	return s.themes.Update(ctx, owner, actor, id, rev, set)
}

// Delete soft-deletes owner's custom theme, freeing its slot. Packaged themes return ErrReadOnly.
func (s *Themes) Delete(ctx context.Context, owner, actor, id string, rev int64) error {
	if isPackaged(id) {
		return ErrReadOnly
	}
	return s.themes.SoftDelete(ctx, owner, actor, id, rev)
}

// Items is the theme repository as limit enforcement sees it.
func (s *Themes) Items() Limited { return s.themes }

// Count returns how many custom themes owner holds.
func (s *Themes) Count(ctx context.Context, owner string) (int64, error) {
	return s.themes.Count(ctx, owner, nil)
}

func buildTheme(in ThemeInput) (data.Theme, error) {
	set, err := patchSet(ThemePatch{
		Name: &in.Name, Style: &in.Style, MainColor: &in.MainColor, Accent1Color: &in.Accent1Color,
		Accent2Color: &in.Accent2Color, Background1Color: &in.Background1Color,
		Background2Color: &in.Background2Color, PrimaryFontName: &in.PrimaryFontName,
		SecondaryFontName: &in.SecondaryFontName, BackgroundImage: &in.BackgroundImage,
	})
	if err != nil {
		return data.Theme{}, err
	}
	str := func(k string) string { v, _ := set[k].(string); return v }
	return data.Theme{
		Name: str("name"), Style: str("style"), MainColor: str("main_color"),
		Accent1Color: str("accent1_color"), Accent2Color: str("accent2_color"),
		Background1Color: str("background1_color"), Background2Color: str("background2_color"),
		PrimaryFontName: str("primary_font_name"), SecondaryFontName: str("secondary_font_name"),
		BackgroundImage: str("background_image"),
	}, nil
}

type themeField struct {
	key  string
	val  *string
	kind int // 0 required text, 1 color, 2 optional https URL
}

const (
	fText = iota
	fColor
	fImage
)

// patchSet validates the non-nil fields of p and returns them keyed by stored field name.
func patchSet(p ThemePatch) (map[string]any, error) {
	fields := []themeField{
		{"name", p.Name, fText}, {"style", p.Style, fText},
		{"main_color", p.MainColor, fColor}, {"accent1_color", p.Accent1Color, fColor},
		{"accent2_color", p.Accent2Color, fColor}, {"background1_color", p.Background1Color, fColor},
		{"background2_color", p.Background2Color, fColor},
		{"primary_font_name", p.PrimaryFontName, fText}, {"secondary_font_name", p.SecondaryFontName, fText},
		{"background_image", p.BackgroundImage, fImage},
	}
	set := map[string]any{}
	for _, f := range fields {
		if f.val == nil {
			continue
		}
		v := strings.TrimSpace(*f.val)
		if err := checkThemeField(f, v); err != nil {
			return nil, err
		}
		set[f.key] = v
	}
	return set, nil
}

func checkThemeField(f themeField, v string) error {
	switch f.kind {
	case fText:
		if v == "" || len(v) > 100 {
			return invalid("theme %s is required and at most 100 characters", f.key)
		}
	case fColor:
		if !hexColor.MatchString(v) {
			return invalid("theme %s must be #RRGGBB or #RRGGBBAA", f.key)
		}
	case fImage:
		if v == "" {
			return nil
		}
		if u, err := url.Parse(v); err != nil || u.Scheme != "https" || u.Host == "" || len(v) > 2048 {
			return invalid("theme %s must be an https URL", f.key)
		}
	}
	return nil
}
