// Package reconcile rebuilds the OpenFGA session tuples from the membership collection, which is
// the source of truth (PADR-0035). It runs as the initiative-reconciler user with the
// openfga-writer role and an operator-minted bearer (PADR-0038). Every pass is idempotent: it
// reads the tuples on each session, diffs them against live memberships, and applies the
// difference, so a failed pass is finished by the next one.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/sweetrpg/initiative-api/data"
)

const (
	scanPage   = 500
	maxBatch   = 100 // auth-api's tuple write limit
	userPrefix = "user:"
	sessionObj = "session:"
)

// ErrTokenRejected means auth-api refused the reconciler's bearer (expired, revoked, or missing
// the openfga-writer role). The pass stops at once; an operator must mint a new token.
var ErrTokenRejected = errors.New("reconcile: reconciler token rejected")

// Tuple is one relationship, as auth-api's tuple endpoints carry it.
type Tuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

// Members is the membership source.
type Members interface {
	ScanMembers(ctx context.Context, q data.Query) (data.Page[data.Member], error)
}

// Tuples is auth-api's internal tuple surface.
type Tuples interface {
	// Read returns every tuple on object.
	Read(ctx context.Context, object string) ([]Tuple, error)
	Write(ctx context.Context, writes, deletes []Tuple) error
}

// Reconciler diffs memberships against tuples.
type Reconciler struct {
	Members Members
	Tuples  Tuples
}

// Result counts one pass.
type Result struct {
	Sessions, Written, Deleted, Failed int
}

var roleRelations = map[string]bool{data.RoleGM: true, data.RoleCoGM: true, data.RolePlayer: true, data.RoleObserver: true}

// desired maps each session that has any membership record to its tuples from live records. A
// session with only deleted records maps to an empty set, so its tuples are removed.
func (r Reconciler) desired(ctx context.Context) (map[string]map[Tuple]bool, error) {
	out := map[string]map[Tuple]bool{}
	cursor := ""
	for {
		page, err := r.Members.ScanMembers(ctx, data.Query{Limit: scanPage, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("scan members: %w", err)
		}
		for _, m := range page.Items {
			obj := sessionObj + m.SessionID
			if out[obj] == nil {
				out[obj] = map[Tuple]bool{}
			}
			if m.DeletedAt != nil || !roleRelations[m.Role] {
				continue
			}
			out[obj][Tuple{User: userPrefix + m.UserID, Relation: m.Role, Object: obj}] = true
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// Run does one pass. It returns ErrTokenRejected if auth-api refuses the token; other failures
// are logged per session and counted, and returned together after every session was tried.
func (r Reconciler) Run(ctx context.Context) (Result, error) {
	want, err := r.desired(ctx)
	if err != nil {
		return Result{}, err
	}
	objects := make([]string, 0, len(want))
	for o := range want {
		objects = append(objects, o)
	}
	sort.Strings(objects)

	res := Result{Sessions: len(objects)}
	for _, obj := range objects {
		w, d, err := r.one(ctx, obj, want[obj])
		res.Written += w
		res.Deleted += d
		if errors.Is(err, ErrTokenRejected) {
			return res, err
		}
		if err != nil {
			res.Failed++
			slog.ErrorContext(ctx, "reconcile session failed", "object", obj, "error", err)
		}
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("reconcile: %d of %d sessions failed", res.Failed, res.Sessions)
	}
	return res, nil
}

// one reconciles a single session object and returns how many tuples it wrote and deleted.
func (r Reconciler) one(ctx context.Context, obj string, want map[Tuple]bool) (written, deleted int, err error) {
	have, err := r.Tuples.Read(ctx, obj)
	if err != nil {
		return 0, 0, err
	}
	haveSet := map[Tuple]bool{}
	var dels []Tuple
	for _, t := range have {
		if !roleRelations[t.Relation] || !strings.HasPrefix(t.User, userPrefix) {
			continue // not a tuple this job owns
		}
		haveSet[t] = true
		if !want[t] {
			dels = append(dels, t)
		}
	}
	var adds []Tuple
	for t := range want {
		if !haveSet[t] {
			adds = append(adds, t)
		}
	}
	sortTuples(adds)
	sortTuples(dels)
	for _, t := range adds {
		slog.InfoContext(ctx, "reconcile: writing missing tuple", "user", t.User, "relation", t.Relation, "object", t.Object)
	}
	for _, t := range dels {
		slog.InfoContext(ctx, "reconcile: deleting extra tuple", "user", t.User, "relation", t.Relation, "object", t.Object)
	}
	// Deletes go first so a role change (old tuple out, new in) never leaves two roles at once.
	for len(dels) > 0 || len(adds) > 0 {
		var bd, ba []Tuple
		bd, dels = take(dels, maxBatch)
		ba, adds = take(adds, maxBatch-len(bd))
		if err := r.Tuples.Write(ctx, ba, bd); err != nil {
			return written, deleted, err
		}
		written += len(ba)
		deleted += len(bd)
	}
	return written, deleted, nil
}

func take(ts []Tuple, n int) (head, rest []Tuple) {
	if n > len(ts) {
		n = len(ts)
	}
	return ts[:n], ts[n:]
}

func sortTuples(ts []Tuple) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		return a.Object+a.Relation+a.User < b.Object+b.Relation+b.User
	})
}
