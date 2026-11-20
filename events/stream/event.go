// Package stream turns committed encounter changes into the events subscribers receive. The data
// layer reports each commit to a Sink (data.Sink); the Hub here is that sink. It renders the
// committed document once per viewer, with the same view builder REST uses, and sends each
// subscriber only what its role may see: ids, never data.
package stream

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/sweetrpg/initiative-api/data"
)

// Source is the publisher name in every envelope.
const Source = "initiative-api"

// Event is one envelope (docs/messaging-nats.md). Data carries ids only: the changed combatant ids
// and, for turn and state changes, the status, round and current combatant id. A client that wants
// the content fetches the encounter, so the content always passes through the view builder.
type Event struct {
	EventID     string    `json:"event_id"`
	OccurredAt  time.Time `json:"occurred_at"`
	Source      string    `json:"source"`
	EncounterID string    `json:"encounter_id"`
	SessionID   string    `json:"session_id,omitempty"`
	// UserID is the addressee of a notice published to the bus (one event per addressee); it is
	// empty on every other event.
	UserID     string `json:"user_id,omitempty"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Action     string `json:"action"`
	Type       string `json:"type"`
	// Revision orders a GM's or co-GM's events. Other viewers never get it: the stored revision
	// moves on every edit, hidden ones included, so it would show them edits they cannot see.
	Revision int64 `json:"revision,omitempty"`
	// ViewTag is, for every other viewer, the digest of the view the change left them with (the same
	// value their encounter view carries). They order events by it and refetch when it differs from
	// the one they hold; a change that leaves it unchanged produces no event for them at all.
	ViewTag string         `json:"view_tag,omitempty"`
	Origin  Origin         `json:"origin"`
	Data    map[string]any `json:"data"`
}

// Origin tags the kind of writer and, to the originating user's own streams only, the client id
// they sent, so a client can recognise its own echo.
type Origin struct {
	Kind     string `json:"kind"`
	ClientID string `json:"client_id,omitempty"`
}

// actionOf is the last segment of an event type ("turn.advanced" is "advanced").
func actionOf(eventType string) string {
	if i := strings.LastIndex(eventType, "."); i >= 0 {
		return eventType[i+1:]
	}
	return eventType
}

// newEventID returns a random (version 4) UUID.
func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// base is the envelope of c with no data and no client id: what every viewer shares.
func base(c data.Change, id string) Event {
	kind := c.Origin.Kind
	if kind == "" {
		kind = data.OriginSystem
	}
	return Event{
		EventID: id, OccurredAt: c.At.UTC(), Source: Source,
		EncounterID: c.After.ID, SessionID: c.SessionID,
		EntityType: "encounter", EntityID: c.After.ID,
		Origin: Origin{Kind: kind},
		Data:   map[string]any{},
	}
}

func typed(e Event, eventType string) Event {
	e.Type, e.Action = eventType, actionOf(eventType)
	return e
}
