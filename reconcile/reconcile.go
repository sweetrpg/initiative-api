// Package reconcile rebuilds the OpenFGA session tuples from the membership collection, which is
// the source of truth (PADR-0035). It runs as the initiative-reconciler user with the
// openfga-writer role and an operator-minted bearer (PADR-0038). Every pass is idempotent: it
// reads the tuples on each session, then that session's current memberships, diffs them, and
// applies the difference, so a failed pass is finished by the next one. Reading tuples first means
// a grant or revoke that lands between the two reads is, at worst, corrected by the next pass
// (two narrow interleavings remain, accepted by PADR-0040 section 4). After the tuple pass it
// collects session mints that never became sessions (PADR-0040 section 1).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sweetrpg/initiative-api/data"
)

const (
	scanPage   = 500
	maxBatch   = 100 // auth-api's tuple write limit
	userPrefix = "user:"
	sessionObj = "session:"
	// mintMinAge keeps garbage collection away from sessions still being created; auth-api
	// compares it against its own clock.
	mintMinAge = 15 * time.Minute
	// maxMintPages bounds the garbage-collection paging loop so a cursor that never ends cannot
	// hold the job until its deadline.
	maxMintPages = 1000
	// defaultGCReserve is the end of the pass's time budget kept for garbage collection, so a slow
	// tuple pass cannot starve it.
	defaultGCReserve = 60 * time.Second
	// maxGMLessPerPass is the most GM-less sessions one pass will remove. More than that looks like
	// a wrong database or an empty members collection, which would otherwise wipe every session
	// older than mintMinAge and its tuples.
	maxGMLessPerPass = 10
)

// mintIDPattern is what auth-api mints. Anything else matches no session, which would read as
// "missing" and close a mint that is not ours to judge.
var mintIDPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

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
	// ScanMembers pages through every membership record, deleted ones included. The pass uses it
	// only to learn which sessions exist.
	ScanMembers(ctx context.Context, q data.Query) (data.Page[data.Member], error)
	// SessionMembers returns one session's membership records, deleted ones included, read fresh.
	SessionMembers(ctx context.Context, sessionID string) ([]data.Member, error)
}

// Mint is an open session mint auth-api lists for garbage collection.
type Mint struct {
	ID string `json:"id"`
}

// Mints is auth-api's mint surface (openfga-writer): open mints at least minAge old, paged by
// the cursor each page returns, and a close that deletes every tuple on the session.
type Mints interface {
	ListMints(ctx context.Context, minAge time.Duration, after string) (mints []Mint, next string, err error)
	CloseMint(ctx context.Context, id string) error
}

// Sessions looks sessions up by ID in the sessions collection, soft-deleted ones included.
type Sessions interface {
	// LookupSessions maps each found ID to whether it is soft-deleted. A missing ID is absent.
	LookupSessions(ctx context.Context, ids []string) (map[string]bool, error)
	// SessionsWithGM returns the IDs among ids that have a live GM member record.
	SessionsWithGM(ctx context.Context, ids []string) (map[string]bool, error)
	// RemoveGMLess soft-deletes a live session that has no GM, and its membership records.
	RemoveGMLess(ctx context.Context, sessionID string) error
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
	// Mints and Sessions enable the mint garbage collection; both nil skips it.
	Mints    Mints
	Sessions Sessions
	// GCReserve is how much of ctx's deadline the tuple pass leaves for garbage collection; zero
	// means 60 seconds. It has no effect when ctx has no deadline.
	GCReserve time.Duration
}

// Result counts one pass.
type Result struct {
	Sessions, Written, Deleted, Failed int
	// MintsClosed counts mints garbage collection closed; MintsSkipped counts those it left for
	// the next pass because the sessions lookup failed.
	MintsClosed, MintsSkipped int
	// GMLessRemoved counts live sessions with no GM that garbage collection soft-deleted.
	GMLessRemoved int
	// GMLessCapped is set when a pass reached maxGMLessPerPass and left further GM-less sessions.
	GMLessCapped bool
}

var roleRelations = map[string]bool{data.RoleGM: true, data.RoleCoGM: true, data.RolePlayer: true, data.RoleObserver: true}

// sessionObjects lists the session objects that have any membership record, live or deleted, so a
// session whose members are all gone is still visited and its tuples removed.
func (r Reconciler) sessionObjects(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := r.Members.ScanMembers(ctx, data.Query{Limit: scanPage, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("scan members: %w", err)
		}
		for _, m := range page.Items {
			seen[m.SessionID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// wanted is the tuple set one session's live memberships call for.
func wanted(sessionID string, members []data.Member) map[Tuple]bool {
	obj := sessionObj + sessionID
	out := map[Tuple]bool{}
	for _, m := range members {
		if m.DeletedAt != nil || !roleRelations[m.Role] {
			continue
		}
		out[Tuple{User: userPrefix + m.UserID, Relation: m.Role, Object: obj}] = true
	}
	return out
}

// Run does one pass. It returns ErrTokenRejected if auth-api refuses the token; other failures
// are logged per session and counted, and returned together after every session was tried.
func (r Reconciler) Run(ctx context.Context) (Result, error) {
	tupleCtx, cancel := r.tuplePassContext(ctx)
	defer cancel()
	ids, err := r.sessionObjects(tupleCtx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Sessions: len(ids)}
	for i, id := range ids {
		if tupleCtx.Err() != nil {
			// The tuple pass's budget is spent: one line and one failure per session left, so the
			// job fails and garbage collection (on the full context) still runs.
			res.Failed += len(ids) - i
			slog.ErrorContext(ctx, "reconcile: tuple pass ran out of time", "done", i, "left", len(ids)-i)
			break
		}
		w, d, err := r.one(tupleCtx, id)
		res.Written += w
		res.Deleted += d
		if errors.Is(err, ErrTokenRejected) {
			return res, err
		}
		if err != nil {
			res.Failed++
			slog.ErrorContext(ctx, "reconcile session failed", "session", id, "error", err)
		}
	}
	// Garbage collection runs on the whole context, not the tuple pass's shorter one.
	if err := r.collect(ctx, &res); err != nil {
		return res, err
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("reconcile: %d failures across %d sessions", res.Failed, res.Sessions)
	}
	return res, nil
}

// tuplePassContext is ctx with its deadline moved earlier by the GC reserve, so the tuple pass
// cannot use the budget garbage collection needs.
func (r Reconciler) tuplePassContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	reserve := r.GCReserve
	if reserve <= 0 {
		reserve = defaultGCReserve
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

// collect closes mints that never became live sessions: for each open mint at least mintMinAge
// old, one sessions lookup that includes soft-deleted records. A mint is closed only when that
// lookup succeeds and returns no record or a deleted one. A failed lookup skips the whole batch
// until the next pass. It pages to the end, because live sessions stay open and would otherwise
// fill the first page and hide a leaked mint.
func (r Reconciler) collect(ctx context.Context, res *Result) error {
	if r.Mints == nil || r.Sessions == nil {
		return nil
	}
	after := ""
	for page := 0; ; page++ {
		if page >= maxMintPages {
			res.Failed++
			slog.ErrorContext(ctx, "reconcile: mint paging did not end", "pages", page)
			return nil
		}
		mints, next, err := r.Mints.ListMints(ctx, mintMinAge, after)
		if err != nil {
			if errors.Is(err, ErrTokenRejected) {
				return err
			}
			res.Failed++
			slog.ErrorContext(ctx, "reconcile: listing session mints failed", "error", err)
			return nil
		}
		if err := r.closeOrphans(ctx, mints, res); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if next == after {
			res.Failed++
			slog.ErrorContext(ctx, "reconcile: auth-api returned the same mint cursor twice", "cursor", next)
			return nil
		}
		after = next
	}
}

func (r Reconciler) closeOrphans(ctx context.Context, mints []Mint, res *Result) error {
	if len(mints) == 0 {
		return nil
	}
	ids := make([]string, 0, len(mints))
	for _, m := range mints {
		if !mintIDPattern.MatchString(m.ID) {
			res.MintsSkipped++
			slog.ErrorContext(ctx, "reconcile: ignoring a mint with a malformed id", "mint", m.ID)
			continue
		}
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	found, err := r.Sessions.LookupSessions(ctx, ids)
	if err != nil {
		// Counted as a failure too: a Mongo outage that persists must fail the job so the stall
		// alert fires, not exit 0 with the mints left open.
		res.MintsSkipped += len(ids)
		res.Failed++
		slog.ErrorContext(ctx, "reconcile: sessions lookup failed; leaving this batch of mints for the next pass", "mints", len(ids), "error", err)
		return nil
	}
	gmless := r.gmLess(ctx, ids, found, res)
	for _, id := range ids {
		if deleted, exists := found[id]; exists && !deleted {
			if !gmless[id] {
				continue // a live session with its GM
			}
			if res.GMLessRemoved >= maxGMLessPerPass {
				if !res.GMLessCapped {
					res.GMLessCapped = true
					res.Failed++
					slog.ErrorContext(ctx, "reconcile: reached the cap of sessions with no GM removed in one pass; leaving the rest", "cap", maxGMLessPerPass)
				}
				continue
			}
			err := r.Sessions.RemoveGMLess(ctx, id)
			if errors.Is(err, data.ErrConflict) {
				slog.InfoContext(ctx, "reconcile: session has a GM or is too new to remove; leaving it", "session", id)
				continue
			}
			if err != nil {
				res.Failed++
				slog.ErrorContext(ctx, "reconcile: removing a session with no GM failed", "session", id, "error", err)
				continue
			}
			res.GMLessRemoved++
			slog.InfoContext(ctx, "reconcile: removed a live session that has no GM", "session", id)
		}
		if err := r.Mints.CloseMint(ctx, id); err != nil {
			if errors.Is(err, ErrTokenRejected) {
				return err
			}
			res.Failed++
			slog.ErrorContext(ctx, "reconcile: closing a mint failed", "mint", id, "error", err)
			continue
		}
		res.MintsClosed++
		slog.InfoContext(ctx, "reconcile: closed a mint with no live session", "mint", id)
	}
	return nil
}

// gmLess returns which of the live sessions among ids have no live GM record, using one query. A
// failed query counts as a failure and returns nothing, so no session is removed on a guess.
func (r Reconciler) gmLess(ctx context.Context, ids []string, found map[string]bool, res *Result) map[string]bool {
	var live []string
	for _, id := range ids {
		if deleted, exists := found[id]; exists && !deleted {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		return nil
	}
	withGM, err := r.Sessions.SessionsWithGM(ctx, live)
	if err != nil {
		res.Failed++
		slog.ErrorContext(ctx, "reconcile: GM lookup failed; leaving live sessions alone", "sessions", len(live), "error", err)
		return nil
	}
	out := map[string]bool{}
	for _, id := range live {
		if !withGM[id] {
			out[id] = true
		}
	}
	return out
}

// one reconciles a single session and returns how many tuples it wrote and deleted. It reads the
// tuples first and the session's members second.
func (r Reconciler) one(ctx context.Context, sessionID string) (written, deleted int, err error) {
	obj := sessionObj + sessionID
	have, err := r.Tuples.Read(ctx, obj)
	if err != nil {
		return 0, 0, err
	}
	members, err := r.Members.SessionMembers(ctx, sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("read members: %w", err)
	}
	want := wanted(sessionID, members)
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
