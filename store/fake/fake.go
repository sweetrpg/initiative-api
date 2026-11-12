// Package fake is an in-memory store.Collection for tests. It evaluates the filter operators the
// data layer emits (equality, $gt/$gte/$lt/$lte/$ne/$in/$regex, $and, $or) and the $set/$unset/
// $inc update operators, and enforces _id and unique-index uniqueness.
package fake

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DB is an in-memory store.Database.
type DB struct {
	mu   sync.Mutex
	cols map[string]*Collection
}

// NewDB returns an empty DB.
func NewDB() *DB { return &DB{cols: map[string]*Collection{}} }

// Collection returns (creating on first use) the named collection.
func (d *DB) Collection(name string) store.Collection { return d.Fake(name) }

// Fake returns the concrete fake collection, for assertions on its contents.
func (d *DB) Fake(name string) *Collection {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cols[name]; ok {
		return c
	}
	c := &Collection{}
	d.cols[name] = c
	return c
}

// Collection is one in-memory collection.
type Collection struct {
	mu      sync.Mutex
	docs    []bson.M
	Indexes []store.IndexSpec
	// FindCalls records each Find's options, so tests can prove limits and sorts reach the query.
	FindCalls []store.FindOptions
}

// Docs returns a copy of the stored documents.
func (c *Collection) Docs() []bson.M {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bson.M(nil), c.docs...)
}

func toM(v any) (bson.M, error) {
	b, err := bson.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m bson.M
	return m, bson.Unmarshal(b, &m)
}

func decode(m bson.M, out any) error {
	b, err := bson.Marshal(m)
	if err != nil {
		return err
	}
	return bson.Unmarshal(b, out)
}

func (c *Collection) InsertOne(_ context.Context, doc any) error {
	m, err := toM(doc)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.violatesUnique(m, -1) {
		return store.ErrDuplicate
	}
	c.docs = append(c.docs, m)
	return nil
}

func (c *Collection) FindOne(_ context.Context, filter bson.D, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.docs {
		if ok, err := match(d, filter); err != nil {
			return err
		} else if ok {
			return decode(d, out)
		}
	}
	return store.ErrNotFound
}

func (c *Collection) Find(_ context.Context, filter bson.D, opts store.FindOptions, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FindCalls = append(c.FindCalls, opts)
	var hits []bson.M
	for _, d := range c.docs {
		ok, err := match(d, filter)
		if err != nil {
			return err
		}
		if ok {
			hits = append(hits, d)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return less(hits[i], hits[j], opts.Sort) })
	if opts.Limit > 0 && int64(len(hits)) > opts.Limit {
		hits = hits[:opts.Limit]
	}
	res := make(bson.A, 0, len(hits))
	for _, h := range hits {
		res = append(res, project(h, opts.Projection))
	}
	return decode(bson.M{"r": res}, &wrap{out})
}

// wrap decodes {"r": [...]} straight into a slice pointer via reflection.
type wrap struct{ out any }

func (w *wrap) UnmarshalBSON(b []byte) error {
	var raw bson.Raw = b
	arr, err := raw.LookupErr("r")
	if err != nil {
		return err
	}
	rv := reflect.ValueOf(w.out).Elem()
	rv.Set(reflect.MakeSlice(rv.Type(), 0, 0))
	vals, err := arr.Array().Values()
	if err != nil {
		return err
	}
	for _, v := range vals {
		item := reflect.New(rv.Type().Elem())
		if err := v.Unmarshal(item.Interface()); err != nil {
			return err
		}
		rv.Set(reflect.Append(rv, item.Elem()))
	}
	return nil
}

func project(d bson.M, proj bson.D) bson.M {
	if len(proj) == 0 {
		return d
	}
	out := bson.M{"_id": d["_id"]}
	for _, e := range proj {
		if v, ok := d[e.Key]; ok {
			out[e.Key] = v
		}
	}
	return out
}

func (c *Collection) FindOneAndUpdate(_ context.Context, filter, update bson.D, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, d := range c.docs {
		ok, err := match(d, filter)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		next := cloneM(d)
		if err := applyUpdate(next, update); err != nil {
			return err
		}
		if c.violatesUnique(next, i) {
			return store.ErrDuplicate
		}
		c.docs[i] = next
		return decode(next, out)
	}
	return store.ErrNotFound
}

func (c *Collection) CountDocuments(_ context.Context, filter bson.D) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, d := range c.docs {
		ok, err := match(d, filter)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func (c *Collection) EnsureIndexes(_ context.Context, specs []store.IndexSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Indexes = append([]store.IndexSpec(nil), specs...)
	return nil
}

// violatesUnique checks _id and every unique index (honoring partial filters) against m,
// ignoring the document at index skip.
func (c *Collection) violatesUnique(m bson.M, skip int) bool {
	for i, d := range c.docs {
		if i == skip {
			continue
		}
		if reflect.DeepEqual(d["_id"], m["_id"]) {
			return true
		}
		for _, ix := range c.Indexes {
			if ix.Unique && sameKey(ix, d, m) {
				return true
			}
		}
	}
	return false
}

func sameKey(ix store.IndexSpec, a, b bson.M) bool {
	for _, d := range []bson.M{a, b} {
		if len(ix.Partial) > 0 {
			if ok, _ := match(d, ix.Partial); !ok {
				return false
			}
		}
	}
	for _, k := range ix.Keys {
		av, bv := a[k.Key], b[k.Key]
		if arr, ok := av.(bson.A); ok { // multikey: any shared element collides
			return anyShared(arr, bv)
		}
		if !reflect.DeepEqual(norm(av), norm(bv)) {
			return false
		}
	}
	return true
}

func anyShared(a bson.A, b any) bool {
	barr, ok := b.(bson.A)
	if !ok {
		return false
	}
	for _, x := range a {
		for _, y := range barr {
			if reflect.DeepEqual(norm(x), norm(y)) {
				return true
			}
		}
	}
	return false
}

func cloneM(m bson.M) bson.M {
	out := make(bson.M, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func applyUpdate(d bson.M, update bson.D) error {
	for _, op := range update {
		fields, ok := op.Value.(bson.D)
		if !ok {
			return fmt.Errorf("fake: update operand for %s must be bson.D", op.Key)
		}
		for _, f := range fields {
			switch op.Key {
			case "$set":
				m, err := toM(bson.D{{Key: "v", Value: f.Value}})
				if err != nil {
					return err
				}
				d[f.Key] = m["v"]
			case "$unset":
				delete(d, f.Key)
			case "$inc":
				cur, _ := toFloat(norm(d[f.Key]))
				inc, _ := toFloat(norm(f.Value))
				d[f.Key] = int64(cur + inc)
			default:
				return fmt.Errorf("fake: unsupported update operator %s", op.Key)
			}
		}
	}
	return nil
}

// norm maps stored and filter values to comparable canonical forms.
func norm(v any) any {
	switch x := v.(type) {
	case time.Time:
		return int64(primitive.NewDateTimeFromTime(x))
	case primitive.DateTime:
		return int64(x)
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
	case *time.Time:
		if x == nil {
			return nil
		}
		return norm(*x)
	}
	return v
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func cmp(a, b any) (int, bool) {
	a, b = norm(a), norm(b)
	if af, ok := toFloat(a); ok {
		if bf, ok := toFloat(b); ok {
			switch {
			case af < bf:
				return -1, true
			case af > bf:
				return 1, true
			}
			return 0, true
		}
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		switch {
		case as < bs:
			return -1, true
		case as > bs:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func less(a, b bson.M, sortD bson.D) bool {
	for _, k := range sortD {
		c, _ := cmp(a[k.Key], b[k.Key])
		if c == 0 {
			continue
		}
		if dir, _ := toFloat(norm(k.Value)); dir < 0 {
			return c > 0
		}
		return c < 0
	}
	return false
}

func match(d bson.M, filter bson.D) (bool, error) {
	for _, e := range filter {
		ok, err := matchElem(d, e)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func matchElem(d bson.M, e bson.E) (bool, error) {
	switch e.Key {
	case "$and", "$or":
		clauses, ok := e.Value.(bson.A)
		if !ok {
			return false, fmt.Errorf("fake: %s needs bson.A", e.Key)
		}
		for _, cl := range clauses {
			sub, ok := cl.(bson.D)
			if !ok {
				return false, fmt.Errorf("fake: %s clause must be bson.D", e.Key)
			}
			m, err := match(d, sub)
			if err != nil {
				return false, err
			}
			if m == (e.Key == "$or") {
				return m, nil
			}
		}
		return e.Key == "$and", nil
	}
	if ops, ok := e.Value.(bson.D); ok && len(ops) > 0 && len(ops[0].Key) > 0 && ops[0].Key[0] == '$' {
		return matchOps(d[e.Key], ops)
	}
	return eq(d[e.Key], e.Value), nil
}

func eq(have, want any) bool {
	if want == nil {
		return have == nil
	}
	if arr, ok := have.(bson.A); ok { // equality against an array field matches any element
		for _, x := range arr {
			if reflect.DeepEqual(norm(x), norm(want)) {
				return true
			}
		}
	}
	return reflect.DeepEqual(norm(have), norm(want))
}

func matchOps(have any, ops bson.D) (bool, error) {
	var pattern, opts string
	for _, o := range ops {
		switch o.Key {
		case "$options":
			opts, _ = o.Value.(string)
		case "$regex":
			pattern, _ = o.Value.(string)
		}
	}
	for _, o := range ops {
		ok, err := matchOp(have, o, pattern, opts)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func matchOp(have any, o bson.E, pattern, opts string) (bool, error) {
	switch o.Key {
	case "$options":
		return true, nil
	case "$ne":
		return !eq(have, o.Value), nil
	case "$in":
		vals, ok := o.Value.(bson.A)
		if !ok {
			return false, fmt.Errorf("fake: $in needs bson.A")
		}
		for _, v := range vals {
			if eq(have, v) {
				return true, nil
			}
		}
		return false, nil
	case "$regex":
		s, ok := have.(string)
		if !ok {
			return false, nil
		}
		if opts == "i" {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false, err
		}
		return re.MatchString(s), nil
	case "$gt", "$gte", "$lt", "$lte":
		c, ok := cmp(have, o.Value)
		if !ok {
			return false, nil
		}
		return (o.Key == "$gt" && c > 0) || (o.Key == "$gte" && c >= 0) ||
			(o.Key == "$lt" && c < 0) || (o.Key == "$lte" && c <= 0), nil
	case "$exists":
		want, _ := o.Value.(bool)
		return (have != nil) == want, nil
	}
	return false, fmt.Errorf("fake: unsupported operator %s", o.Key)
}
