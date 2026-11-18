package data

import (
	"context"
	"log/slog"
	"regexp"
	"time"
)

// Event types an encounter commit can produce. The default for a write whose caller named none is
// EventUpdated, which carries no ids, so an unannotated write is safe: subscribers refetch.
const (
	EventCreated           = "encounter.created"
	EventUpdated           = "encounter.updated"
	EventDeleted           = "encounter.deleted"
	EventReleased          = "encounter.released"
	EventStarted           = "encounter.started"
	EventEnded             = "encounter.ended"
	EventReset             = "encounter.reset"
	EventTurn              = "turn.advanced"
	EventCombatantAdded    = "combatant.added"
	EventCombatantUpdated  = "combatant.updated"
	EventCombatantRemoved  = "combatant.removed"
	EventCombatantRestored = "combatant.restored"
	EventControllers       = "controllers.updated"
)

// Origin says who made a change, so a client can recognise the echo of its own write. The actor
// is the originating user (never serialized); ClientID is the opaque value the client sent in
// X-Initiative-Client-Id, already validated.
type Origin struct {
	Kind     string // user, integration, or system
	ClientID string
	ActorID  string
}

// Origin kinds.
const (
	OriginUser        = "user"
	OriginIntegration = "integration"
	OriginSystem      = "system"
)

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidClientID reports whether v may be recorded as an origin client id. A bad value is ignored,
// never rejected.
func ValidClientID(v string) bool { return clientIDPattern.MatchString(v) }

type opKey struct{}
type originKey struct{}

type op struct {
	typ string
	ids []string
}

// WithOp names the event type the next encounter commit on ctx produces, and the combatants it
// changed. Domain entry points set it; a commit with none is EventUpdated with no ids.
func WithOp(ctx context.Context, eventType string, combatantIDs ...string) context.Context {
	return context.WithValue(ctx, opKey{}, op{eventType, combatantIDs})
}

// WithOrigin records who is making the writes on ctx.
func WithOrigin(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}

func opOf(ctx context.Context) op {
	o, _ := ctx.Value(opKey{}).(op)
	if o.typ == "" {
		o.typ = EventUpdated
	}
	return o
}

func originOf(ctx context.Context) Origin {
	o, _ := ctx.Value(originKey{}).(Origin)
	if o.Kind == "" {
		o.Kind = OriginSystem
	}
	return o
}

// Change is one committed encounter write, handed to the Sink right after the commit. After is the
// stored document as the write left it, so a sink can render it for each subscriber without a
// read; SessionID is the session the encounter belonged to before the write (it differs from
// After.SessionID only for a release), so the audience of a release or delete is the session's.
// OwnerID is the encounter's owner after the write.
type Change struct {
	Type  string
	After Encounter
	// Before is the document as it was just before the write, when the write path had it at the
	// revision the write was made against; nil when unknown. A subscriber's view is compared before
	// and after, so a change it cannot see produces no event for it.
	Before      *Encounter
	SessionID   string
	OwnerID     string
	CombatantID []string
	// GMOnly marks a change only GM-level viewers may hear of: a limit or entitlement flag (lock,
	// inactive) changed. It never reaches a player or observer, and never the bus.
	GMOnly bool
	Origin Origin
	At     time.Time
}

// Sink receives every committed encounter change after the write has succeeded. Emit must not
// block. Changes made by concurrent writers can reach the sink out of commit order, so clients
// order what they receive by the revision (GM and co-GM) or the view_tag (everyone else) in each
// event, not by arrival.
type Sink interface {
	Emit(ctx context.Context, c Change)
}

// SetSink installs the sink for every encounter commit made through r. It is set once at
// startup; with none, commits emit nothing.
func (r *Encounters) SetSink(s Sink) {
	r.sink = s
	r.commit = func(ctx context.Context, kind commitKind, before, after *Encounter) {
		r.emitCommit(ctx, kind, before, *after)
	}
}

type commitKind int

const (
	commitCreate commitKind = iota
	commitWrite
	commitDelete
	commitFlag
)

func (r *Encounters) emitCommit(ctx context.Context, kind commitKind, before *Encounter, after Encounter) {
	if r.sink == nil {
		return
	}
	c := Change{After: after, Before: before, SessionID: after.SessionID, OwnerID: after.OwnerUserID, Origin: originOf(ctx), At: r.now()}
	switch kind {
	case commitCreate:
		c.Type = EventCreated
	case commitDelete:
		c.Type = EventDeleted
	case commitFlag:
		c.Type, c.GMOnly = EventUpdated, true
	default:
		o := opOf(ctx)
		c.Type, c.CombatantID = o.typ, o.ids
	}
	r.deliver(ctx, c)
}

// deliver hands c to the sink. A sink that panics must never fail a write that already
// committed, so the panic is recovered and logged.
func (r *Encounters) deliver(ctx context.Context, c Change) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "an encounter event sink panicked", "encounter", c.After.ID, "type", c.Type, "panic", p)
		}
	}()
	r.sink.Emit(ctx, c)
}

// emit sends a change the caller built itself (scrub, release, retract), for the cases the
// generic commit hook does not cover.
func (r *Encounters) emit(ctx context.Context, c Change) {
	if r.sink == nil {
		return
	}
	c.Origin, c.At = originOf(ctx), r.now()
	r.deliver(ctx, c)
}
