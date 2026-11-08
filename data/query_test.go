package data

import (
	"errors"
	"fmt"
	"testing"
)

func seedRosters(t *testing.T, r *Owned[Roster, *Roster], owner string, names ...string) {
	t.Helper()
	for _, n := range names {
		mustCreate(t, r, owner, n)
	}
}

func names(p Page[Roster]) []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Name
	}
	return out
}

func TestListIsOwnerScoped(t *testing.T) {
	r, _ := newRosters(t)
	seedRosters(t, r, "u1", "mine1", "mine2")
	seedRosters(t, r, "u2", "theirs")
	p, err := r.List(ctx, "u1", Query{})
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("items=%v err=%v, want 2 of u1's", names(p), err)
	}
}

func TestListFilters(t *testing.T) {
	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"eq", Filter{"name", OpEq, []any{"Orcs"}}, []string{"Orcs"}},
		{"in", Filter{"name", OpIn, []any{"Orcs", "Elves"}}, []string{"Elves", "Orcs"}},
		{"contains is case-insensitive", Filter{"name", OpContains, []any{"gob"}}, []string{"Goblins"}},
		{"contains escapes regex", Filter{"name", OpContains, []any{".*"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newRosters(t)
			seedRosters(t, r, "u1", "Goblins", "Orcs", "Elves")
			p, err := r.List(ctx, "u1", Query{Filters: []Filter{tt.filter}, Sort: []SortKey{{Field: "name"}}})
			if err != nil || fmt.Sprint(names(p)) != fmt.Sprint(tt.want) {
				t.Fatalf("got %v err=%v, want %v", names(p), err, tt.want)
			}
		})
	}
}

func TestListRejectsBadQueries(t *testing.T) {
	tests := []struct {
		name string
		q    Query
	}{
		{"unlisted filter field", Query{Filters: []Filter{{"owner_user_id", OpEq, []any{"u2"}}}}},
		{"operator-looking field", Query{Filters: []Filter{{"$where", OpEq, []any{"1"}}}}},
		{"nested document value", Query{Filters: []Filter{{"name", OpEq, []any{map[string]any{"$ne": ""}}}}}},
		{"unknown operator", Query{Filters: []Filter{{"name", Op("regex"), []any{"x"}}}}},
		{"empty contains", Query{Filters: []Filter{{"name", OpContains, []any{""}}}}},
		{"eq with two values", Query{Filters: []Filter{{"name", OpEq, []any{"a", "b"}}}}},
		{"unsortable field", Query{Sort: []SortKey{{Field: "characters"}}}},
		{"repeated sort field", Query{Sort: []SortKey{{Field: "name"}, {Field: "name"}}}},
		{"unprojectable field", Query{Fields: []string{"password"}}},
		{"negative limit", Query{Limit: -1}},
		{"garbage cursor", Query{Cursor: "!!!"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newRosters(t)
			if _, err := r.List(ctx, "u1", tt.q); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLimitAndSortReachTheDatabase(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantFetch int64
	}{
		{"default", 0, DefaultLimit + 1},
		{"explicit", 3, 4},
		{"clamped to max", 10_000, MaxLimit + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, col := newRosters(t)
			if _, err := r.List(ctx, "u1", Query{Limit: tt.limit, Sort: []SortKey{{Field: "name", Desc: true}}}); err != nil {
				t.Fatal(err)
			}
			got := col.FindCalls[0]
			if got.Limit != tt.wantFetch || got.Sort[0].Key != "name" || got.Sort[len(got.Sort)-1].Key != "_id" {
				t.Fatalf("find options = %+v, want limit %d, sort name then _id", got, tt.wantFetch)
			}
		})
	}
}

func TestPagingWalksEveryRecordOnce(t *testing.T) {
	tests := []struct {
		name string
		sort []SortKey
		want string
	}{
		{"ascending", []SortKey{{Field: "name"}}, "[a b c d e]"},
		{"descending", []SortKey{{Field: "name", Desc: true}}, "[e d c b a]"},
		{"tie on first key falls to id", []SortKey{{Field: "favorite"}, {Field: "name"}}, "[a b c d e]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newRosters(t)
			seedRosters(t, r, "u1", "c", "a", "e", "b", "d")
			var all []string
			cursor := ""
			for pages := 0; pages < 10; pages++ {
				p, err := r.List(ctx, "u1", Query{Sort: tt.sort, Limit: 2, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, names(p)...)
				if cursor = p.NextCursor; cursor == "" {
					break
				}
			}
			if fmt.Sprint(all) != tt.want {
				t.Fatalf("walked %v, want %s", all, tt.want)
			}
		})
	}
}

func TestLastPageHasNoCursor(t *testing.T) {
	r, _ := newRosters(t)
	seedRosters(t, r, "u1", "a", "b")
	p, _ := r.List(ctx, "u1", Query{Limit: 2})
	if p.NextCursor != "" {
		t.Fatalf("cursor = %q on a full-fit page, want none", p.NextCursor)
	}
}

func TestCursorFromDifferentSortIsRejected(t *testing.T) {
	r, _ := newRosters(t)
	seedRosters(t, r, "u1", "a", "b", "c")
	p, _ := r.List(ctx, "u1", Query{Sort: []SortKey{{Field: "name"}}, Limit: 1})
	_, err := r.List(ctx, "u1", Query{Sort: []SortKey{{Field: "name"}, {Field: "favorite"}}, Cursor: p.NextCursor})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestProjectionLimitsFetchedFields(t *testing.T) {
	r, col := newRosters(t)
	seedRosters(t, r, "u1", "a")
	p, err := r.List(ctx, "u1", Query{Fields: []string{"name"}})
	if err != nil || p.Items[0].Name != "a" || p.Items[0].OwnerUserID != "" {
		t.Fatalf("item=%+v err=%v, want name only", p.Items, err)
	}
	if len(col.FindCalls[0].Projection) == 0 {
		t.Fatal("projection did not reach the database")
	}
}

func TestCountIsOwnerScopedAndFiltered(t *testing.T) {
	r, _ := newRosters(t)
	seedRosters(t, r, "u1", "Goblins", "Orcs")
	seedRosters(t, r, "u2", "Goblins")
	tests := []struct {
		name    string
		filters []Filter
		want    int64
	}{
		{"all own", nil, 2},
		{"filtered", []Filter{{"name", OpEq, []any{"Goblins"}}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := r.Count(ctx, "u1", tt.filters)
			if err != nil || n != tt.want {
				t.Fatalf("count=%d err=%v, want %d", n, err, tt.want)
			}
		})
	}
}

func TestListExcludesSoftDeleted(t *testing.T) {
	r, _ := newRosters(t)
	seedRosters(t, r, "u1", "a", "b")
	_ = r.SoftDelete(ctx, "u1", "u1", "a", 1)
	p, _ := r.List(ctx, "u1", Query{})
	if len(p.Items) != 1 || p.Items[0].Name != "b" {
		t.Fatalf("items = %v, want only b", names(p))
	}
}

func TestEqualSortValuesPageByIDTieBreak(t *testing.T) {
	r, _ := newRosters(t) // ids are assigned a, b, c, d in creation order
	seedRosters(t, r, "u1", "same", "same", "same", "other")
	var ids []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		p, err := r.List(ctx, "u1", Query{Sort: []SortKey{{Field: "name"}}, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			ids = append(ids, it.ID)
		}
		if cursor = p.NextCursor; cursor == "" {
			break
		}
	}
	if fmt.Sprint(ids) != "[d a b c]" {
		t.Fatalf("walked ids %v, want [d a b c] (other, then the three equal names by id)", ids)
	}
}
