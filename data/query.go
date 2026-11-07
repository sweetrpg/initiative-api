package data

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// List bounds.
const (
	DefaultLimit = 50
	MaxLimit     = 200
	maxInValues  = 100
	maxValueLen  = 128
)

// Op is a filter operator. Clients choose from this closed set; a client string never becomes an
// operator key or a live regular expression (PADR-0030).
type Op string

// Filter operators.
const (
	OpEq       Op = "eq"
	OpIn       Op = "in"
	OpContains Op = "contains"
)

// Filter restricts a list to records whose Field matches. eq takes one value, in takes up to
// maxInValues, contains takes one non-empty string and matches case-insensitively as a literal.
type Filter struct {
	Field  string
	Op     Op
	Values []any
}

// SortKey orders a list. _id is always appended as the final tie-break so paging is stable.
type SortKey struct {
	Field string
	Desc  bool
}

// Query is the query-layer shape of a list call: everything here is applied by the database.
// Limit 0 means DefaultLimit; larger than MaxLimit is clamped. Fields is an optional projection
// (empty returns whole documents). Cursor is the opaque NextCursor of the previous page.
type Query struct {
	Filters []Filter
	Sort    []SortKey
	Fields  []string
	Limit   int
	Cursor  string
}

// Page is one page of results; NextCursor is empty on the last page.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func in(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// scalar admits only plain values, so a nested document can never smuggle in an operator.
func scalar(v any) (any, bool) {
	switch x := v.(type) {
	case string, bool, int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
	}
	return nil, false
}

func filterClause(spec Spec, f Filter) (bson.D, error) {
	if !in(spec.Filterable, f.Field) {
		return nil, invalid("cannot filter by %q", f.Field)
	}
	vals := make(bson.A, 0, len(f.Values))
	for _, v := range f.Values {
		s, ok := scalar(v)
		if !ok {
			return nil, invalid("filter %q has a non-scalar value", f.Field)
		}
		vals = append(vals, s)
	}
	switch f.Op {
	case OpEq:
		if len(vals) != 1 {
			return nil, invalid("eq on %q needs exactly one value", f.Field)
		}
		return bson.D{{Key: f.Field, Value: vals[0]}}, nil
	case OpIn:
		if len(vals) == 0 || len(vals) > maxInValues {
			return nil, invalid("in on %q needs 1 to %d values", f.Field, maxInValues)
		}
		return bson.D{{Key: f.Field, Value: bson.D{{Key: "$in", Value: vals}}}}, nil
	case OpContains:
		s, ok := firstString(vals)
		if len(vals) != 1 || !ok || s == "" || len(s) > maxValueLen {
			return nil, invalid("contains on %q needs one non-empty string up to %d bytes", f.Field, maxValueLen)
		}
		return bson.D{{Key: f.Field, Value: bson.D{{Key: "$regex", Value: regexp.QuoteMeta(s)}, {Key: "$options", Value: "i"}}}}, nil
	}
	return nil, invalid("unknown operator %q", f.Op)
}

func firstString(v bson.A) (string, bool) {
	if len(v) == 0 {
		return "", false
	}
	s, ok := v[0].(string)
	return s, ok
}

func sortKeys(spec Spec, in2 []SortKey) ([]SortKey, error) {
	keys := append([]SortKey(nil), in2...)
	seen := map[string]bool{}
	for _, k := range keys {
		if k.Field != "_id" && !in(spec.Sortable, k.Field) {
			return nil, invalid("cannot sort by %q", k.Field)
		}
		if seen[k.Field] {
			return nil, invalid("sort field %q repeated", k.Field)
		}
		seen[k.Field] = true
	}
	if !seen["_id"] {
		keys = append(keys, SortKey{Field: "_id"})
	}
	return keys, nil
}

func projection(spec Spec, fields []string, keys []SortKey) (bson.D, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	p := bson.D{}
	added := map[string]bool{"_id": true}
	for _, f := range fields {
		if !in(spec.Projectable, f) {
			return nil, invalid("cannot project %q", f)
		}
		if !added[f] {
			added[f] = true
			p = append(p, bson.E{Key: f, Value: 1})
		}
	}
	for _, k := range keys { // the cursor needs the sort values of the last row
		if !added[k.Field] {
			added[k.Field] = true
			p = append(p, bson.E{Key: k.Field, Value: 1})
		}
	}
	return p, nil
}

type plan struct {
	filter bson.D
	opts   store.FindOptions
	keys   []SortKey
	limit  int
}

func buildPlan(spec Spec, base bson.D, q Query) (plan, error) {
	if q.Limit < 0 {
		return plan{}, invalid("limit must not be negative")
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)

	keys, err := sortKeys(spec, q.Sort)
	if err != nil {
		return plan{}, err
	}
	proj, err := projection(spec, q.Fields, keys)
	if err != nil {
		return plan{}, err
	}
	clauses, err := clausesFor(spec, base, q)
	if err != nil {
		return plan{}, err
	}
	if q.Cursor != "" {
		vals, err := decodeCursor(q.Cursor, len(keys))
		if err != nil {
			return plan{}, err
		}
		clauses = append(clauses, after(keys, vals))
	}
	sortD := make(bson.D, 0, len(keys))
	for _, k := range keys {
		dir := 1
		if k.Desc {
			dir = -1
		}
		sortD = append(sortD, bson.E{Key: k.Field, Value: dir})
	}
	// Fetch one extra row to learn whether another page exists.
	return plan{
		filter: bson.D{{Key: "$and", Value: clauses}},
		opts:   store.FindOptions{Sort: sortD, Projection: proj, Limit: int64(limit + 1)},
		keys:   keys,
		limit:  limit,
	}, nil
}

func clausesFor(spec Spec, base bson.D, q Query) (bson.A, error) {
	clauses := bson.A{}
	if len(base) > 0 {
		clauses = append(clauses, base)
	}
	for _, f := range q.Filters {
		c, err := filterClause(spec, f)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, c)
	}
	return clauses, nil
}

// after builds the keyset condition for rows strictly after vals in sort order.
func after(keys []SortKey, vals []any) bson.D {
	ors := make(bson.A, 0, len(keys))
	for i := range keys {
		and := bson.D{}
		for j := 0; j < i; j++ {
			and = append(and, bson.E{Key: keys[j].Field, Value: vals[j]})
		}
		op := "$gt"
		if keys[i].Desc {
			op = "$lt"
		}
		and = append(and, bson.E{Key: keys[i].Field, Value: bson.D{{Key: op, Value: vals[i]}}})
		ors = append(ors, and)
	}
	return bson.D{{Key: "$or", Value: ors}}
}

func encodeCursor(vals []any) (string, error) {
	b, err := bson.MarshalExtJSON(bson.M{"v": bson.A(vals)}, true, false)
	return base64.RawURLEncoding.EncodeToString(b), err
}

func decodeCursor(c string, want int) ([]any, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return nil, invalid("malformed cursor")
	}
	var m bson.M
	if err := bson.UnmarshalExtJSON(b, true, &m); err != nil {
		return nil, invalid("malformed cursor")
	}
	arr, ok := m["v"].(primitive.A)
	if !ok || len(arr) != want {
		return nil, invalid("cursor does not match this query's sort")
	}
	for _, v := range arr {
		switch v.(type) {
		case string, int32, int64, float64, bool, primitive.DateTime:
		default:
			return nil, invalid("malformed cursor")
		}
	}
	return arr, nil
}

// cursorFor encodes the sort values of the last row on a page.
func cursorFor(item any, keys []SortKey) (string, error) {
	raw, err := bson.Marshal(item)
	if err != nil {
		return "", err
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	vals := make([]any, len(keys))
	for i, k := range keys {
		v, ok := m[k.Field]
		if !ok || v == nil {
			return "", invalid("cannot page: sort field %q is missing on a record", k.Field)
		}
		vals[i] = v
	}
	return encodeCursor(vals)
}

// runQuery applies q entirely in the database: filter, sort, projection, limit, and keyset
// cursor. base is the mandatory scope (owner and live-record filters) every caller supplies.
func runQuery[T any](ctx context.Context, c store.Collection, spec Spec, base bson.D, q Query) (Page[T], error) {
	p, err := buildPlan(spec, base, q)
	if err != nil {
		return Page[T]{}, err
	}
	var items []T
	if err := c.Find(ctx, p.filter, p.opts, &items); err != nil {
		return Page[T]{}, err
	}
	if len(items) <= p.limit {
		return Page[T]{Items: items}, nil
	}
	items = items[:p.limit]
	next, err := cursorFor(items[p.limit-1], p.keys)
	if err != nil {
		return Page[T]{}, err
	}
	return Page[T]{Items: items, NextCursor: next}, nil
}

// List returns one page of owner's live records.
func (r *Owned[T, P]) List(ctx context.Context, ownerUserID string, q Query) (Page[T], error) {
	if ownerUserID == "" {
		return Page[T]{}, invalid("owner is required")
	}
	return runQuery[T](ctx, r.c, r.spec, scope(ownerUserID), q)
}

// Count returns how many of owner's live records match filters, counted by the database.
func (r *Owned[T, P]) Count(ctx context.Context, ownerUserID string, filters []Filter) (int64, error) {
	if ownerUserID == "" {
		return 0, invalid("owner is required")
	}
	clauses, err := clausesFor(r.spec, scope(ownerUserID), Query{Filters: filters})
	if err != nil {
		return 0, err
	}
	return r.c.CountDocuments(ctx, bson.D{{Key: "$and", Value: clauses}})
}
