package data

import (
	"context"
	"regexp"
	"time"
)

// Event types an encounter commit can produce. The default for a write whose caller named none is
// EventUpdated, which carries no ids, so an unannotated write is safe: subscribers refetch.
const (
	EventCreated     = "encounter.created"
	EventUpdated     = "encounter.updated"
	EventDeleted     = "encounter.deleted"
	EventReleased    = "encounter.released"
	EventStarted     = "encounter.started"
	EventEnded       = "encounter.ended"
	EventReset       = "encounter.reset"
	EventTurn        = "turn.advanced"
	EventCombatantAd = "combatant.added"
	EventCombatantUp = "combatant.updated"
	EventCombatantRm = "combatant.removed"
	EventCombatantRs = "combatant.restored"
	EventControllers = "controllers.updated"
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
	Type        string
	After       Encounter
	SessionID   string
	OwnerID     string
	CombatantID []string
	// GMOnly marks a write only the owner or GM may hear of: a limit or entitlement flag changed
	// (lock, inactive). It never reaches players, co-GMs' bus consumers, or the bus.
	GMOnly bool
	Origin Origin
	At     time.Time
}

// Sink receives every committed encounter change, in commit order per caller, after the write has
// succeeded. Emit must not block and must not fail the write; it recovers from its own errors.
type Sink interface {
	Emit(ctx context.Context, c Change)
}

// SetSink installs the sink for every encounter commit made through r. It is set once at
// startup; with none, commits emit nothing.
func (r *Encounters) SetSink(s Sink) {
	r.sink = s
	r.commit = func(ctx context.Context, kind commitKind, after any) {
		if e, ok := after.(*Encounter); ok {
			r.emitCommit(ctx, kind, *e)
		}
	}
}

type commitKind int

const (
	commitCreate commitKind = iota
	commitWrite
	commitDelete
	commitFlag
)

func (r *Encounters) emitCommit(ctx context.Context, kind commitKind, after Encounter) {
	if r.sink == nil {
		return
	}
	c := Change{After: after, SessionID: after.SessionID, OwnerID: after.OwnerUserID, Origin: originOf(ctx), At: r.now()}
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
	r.sink.Emit(ctx, c)
}

// emit sends a change the caller built itself (scrub, release, retract), for the cases the
// generic commit hook does not cover.
func (r *Encounters) emit(ctx context.Context, c Change) {
	if r.sink == nil {
		return
	}
	c.Origin, c.At = originOf(ctx), r.now()
	r.sink.Emit(ctx, c)
}
