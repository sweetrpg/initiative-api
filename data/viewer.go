package data

import (
	"context"
	"errors"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// Viewer is the proof of how a caller may see one encounter. Every encounter view is built from a
// Viewer and nothing else, so the view builder never recomputes membership (PADR-0035). The only
// ways to get a granted Viewer are AuthorizeView (a session member, asked of OpenFGA with the
// caller's own bearer) and an owner-scoped load of a private encounter. The zero value grants
// nothing.
type Viewer struct {
	actor, session string
	level          ViewLevel
}

// ViewLevel is the least a Viewer may see, weakest first. The zero value sees nothing.
type ViewLevel int

// View levels: Observer and Player both get the non-GM view; only the Player controls combatants.
const (
	ViewNone ViewLevel = iota
	ViewObserver
	ViewPlayer
	ViewGM // gm, co_gm, or the owner of a private encounter
)

// Actor returns the user the viewer is.
func (v Viewer) Actor() string { return v.actor }

// SessionID returns the session the viewer covers; empty for the owner of a private encounter.
func (v Viewer) SessionID() string { return v.session }

// Level returns what the viewer may see.
func (v Viewer) Level() ViewLevel { return v.level }

// BusViewer is the viewer the platform-bus form of an event is rendered for: observer level, no
// actor, bound to the session of the encounter being rendered ("" for a private encounter). It
// grants no read: nothing can load an encounter with it, it only lets the view builder decide what
// an unprivileged consumer may be told about a document already in memory.
func BusViewer(sessionID string) Viewer { return Viewer{session: sessionID, level: ViewObserver} }

// ownerViewer is the viewer for a private encounter the caller owns.
func ownerViewer(owner string) Viewer { return Viewer{actor: owner, level: ViewGM} }

// AuthorizeView returns the Viewer for actor on the session. OpenFGA decides membership (can_view,
// PADR-0035) and the actor's live member record names the role; the role's own relation is then
// checked too, and the two must agree or the answer is ErrForbidden and the zero Viewer. A caller
// who is not a member, or whose record and tuples disagree (for example during a role change),
// learns nothing else. When no answer can be obtained the error is relations.ErrUnavailable or
// relations.ErrUnauthorized, so session data fails closed.
func (r *Sessions) AuthorizeView(ctx context.Context, actor, sessionID string) (Viewer, error) {
	d, err := Authorize(ctx, actor, sessionID, relView)
	if errors.Is(err, ErrForbidden) {
		return Viewer{}, ErrNotMember
	}
	if err != nil {
		return Viewer{}, err
	}
	m, err := r.Member(ctx, d, actor)
	if errors.Is(err, ErrNotFound) {
		return Viewer{}, ErrNotMember
	}
	if err != nil {
		return Viewer{}, err
	}
	var relation string
	var level ViewLevel
	switch m.Role {
	case RoleGM, RoleCoGM:
		relation, level = relEdit, ViewGM
	case RolePlayer:
		relation, level = RolePlayer, ViewPlayer
	case RoleObserver:
		relation, level = RoleObserver, ViewObserver
	default:
		return Viewer{}, ErrNotMember
	}
	if _, err := Authorize(ctx, actor, sessionID, relation); errors.Is(err, ErrForbidden) {
		return Viewer{}, ErrNotMember
	} else if err != nil {
		return Viewer{}, err
	}
	return Viewer{actor: d.actor, session: d.session, level: level}, nil
}

// LoadOwnedView reads the owner's private encounter with the viewer to render it with. An
// encounter that belongs to a session is not served this way: its owner reaches it through the
// session like any other GM (AuthorizeView), so a removed GM keeps no side door.
func (r *Encounters) LoadOwnedView(ctx context.Context, owner, id string) (Encounter, Viewer, error) {
	e, err := r.Load(ctx, owner, id)
	if err != nil {
		return Encounter{}, Viewer{}, err
	}
	if e.SessionID != "" {
		return Encounter{}, Viewer{}, ErrForbidden
	}
	return e, ownerViewer(owner), nil
}

// LoadForViewer reads one encounter of the viewer's session. A zero viewer, or an encounter in
// another session, is not found: the answer never distinguishes the two.
func (r *Encounters) LoadForViewer(ctx context.Context, v Viewer, id string) (Encounter, error) {
	if v.level == ViewNone || v.session == "" {
		return Encounter{}, ErrForbidden
	}
	id, ok := cleanID(id)
	if !ok {
		return Encounter{}, ErrNotFound
	}
	var e Encounter
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "session_id", Value: v.session}, {Key: "deleted_at", Value: nil}}, &e)
	if errors.Is(err, store.ErrNotFound) {
		return Encounter{}, ErrNotFound
	}
	return e, err
}

// ListForViewer reads the viewer's session's encounters in display order, at most limit (the
// store default when zero).
func (r *Encounters) ListForViewer(ctx context.Context, v Viewer, limit int64) ([]Encounter, error) {
	if v.level == ViewNone || v.session == "" {
		return nil, ErrForbidden
	}
	var out []Encounter
	err := r.c.Find(ctx, bson.D{{Key: "session_id", Value: v.session}, {Key: "deleted_at", Value: nil}},
		store.FindOptions{Sort: bson.D{{Key: "position", Value: 1}, {Key: "_id", Value: 1}}, Limit: limit}, &out)
	return out, err
}

// ListOwnedViews returns one page of owner's private encounters (those with no session) and the
// viewer to render them with. Filtering on session_id happens in the query, so pages are full and
// the cursor is stable; session encounters are read through the session.
func (r *Encounters) ListOwnedViews(ctx context.Context, owner string, q Query) (Page[Encounter], Viewer, error) {
	if err := checkUserID(owner); err != nil {
		return Page[Encounter]{}, Viewer{}, err
	}
	base := append(scope(owner), bson.E{Key: "session_id", Value: nil})
	page, err := runQuery[Encounter](ctx, r.c, r.spec, base, q)
	return page, ownerViewer(owner), err
}

// OwnedViewer returns the viewer for rendering e, an encounter the caller has just read or written
// on the owner path. It refuses an encounter that is not the owner's or that belongs to a session,
// so an owner viewer is never issued for data the owner path may not show.
func (r *Encounters) OwnedViewer(owner string, e Encounter) (Viewer, error) {
	if checkUserID(owner) != nil || e.OwnerUserID != owner || e.SessionID != "" {
		return Viewer{}, ErrForbidden
	}
	return ownerViewer(owner), nil
}

// RequireSystem rejects a game system that has no definition.
func (r *Encounters) RequireSystem(ctx context.Context, sysID string) error {
	return r.requireSystem(ctx, sysID)
}
