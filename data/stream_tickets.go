package data

import (
	"context"
	"errors"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CollectionStreamTickets holds the one-time tickets that open a WebSocket for a browser, which
// cannot send an Authorization header on an upgrade.
const CollectionStreamTickets = "stream_tickets"

// ticketRowTTL is the TTL index on a ticket row. MongoDB removes expired rows about once a minute,
// so Redeem checks the age itself; the index only keeps the collection small.
const ticketRowTTL = 2 * time.Minute

var ticketIndexes = []store.IndexSpec{{Name: "at_ttl", Keys: bson.D{{Key: "at", Value: 1}}, ExpireAfter: ticketRowTTL}}

// StreamTicket is what a minted ticket stands for. Only the SHA-256 of the ticket is stored, as the
// row id, so a database read does not yield a usable ticket. Nothing here is trusted beyond who
// asked, for what, and at what level: access is re-read when the ticket is redeemed.
type StreamTicket struct {
	ID          string    `bson:"_id"` // hex SHA-256 of the ticket
	UserID      string    `bson:"user_id"`
	SessionID   string    `bson:"session_id,omitempty"` // empty: the user's private encounters
	EncounterID string    `bson:"encounter_id,omitempty"`
	Level       ViewLevel `bson:"level"` // what the user could see when the ticket was minted
	ClientID    string    `bson:"client_id,omitempty"`
	At          time.Time `bson:"at"`
}

// StreamTickets is the ticket store.
type StreamTickets struct {
	c   store.Collection
	now func() time.Time
}

// NewStreamTickets returns the ticket store on db.
func NewStreamTickets(db store.Database) *StreamTickets {
	return &StreamTickets{c: db.Collection(CollectionStreamTickets), now: time.Now}
}

// Mint stores t (its ID already the hash of the ticket handed to the caller).
func (r *StreamTickets) Mint(ctx context.Context, t StreamTicket) error {
	if err := checkUserID(t.UserID); err != nil {
		return err
	}
	t.At = r.now()
	return r.c.InsertOne(ctx, t)
}

// Redeem spends the ticket whose hash is id: it finds the row, then deletes it by _id and requires
// that the delete removed one row. Two redeems can both find the row, but only one delete removes
// it (the other reports ErrNotFound, which store.DeleteOne raises on a deleted count of zero), so a
// ticket opens one stream even when two pods are asked at once.
// A ticket older than maxAge, unknown, or already spent is ErrNotFound.
func (r *StreamTickets) Redeem(ctx context.Context, id string, maxAge time.Duration) (StreamTicket, error) {
	var t StreamTicket
	if err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}}, &t); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return StreamTicket{}, ErrNotFound
		}
		return StreamTicket{}, err
	}
	if err := r.c.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return StreamTicket{}, ErrNotFound
		}
		return StreamTicket{}, err
	}
	if r.now().Sub(t.At) > maxAge {
		return StreamTicket{}, ErrNotFound
	}
	return t, nil
}

// TicketView returns the Viewer a redeemed ticket opens: the user's live member record decides, not
// the ticket. OpenFGA was asked when the ticket was minted and is not asked again (a redeem carries
// no bearer to ask with), so the live record is the check: the session must exist, the user must
// still be a member, and their role's level must not exceed minted (a member promoted since gets a
// new ticket). A lower level is served at the lower level. Every refusal is ErrNotMember.
func (r *Sessions) TicketView(ctx context.Context, userID, sessionID string, minted ViewLevel) (Viewer, error) {
	id, ok := cleanID(sessionID)
	if !ok || checkUserID(userID) != nil {
		return Viewer{}, ErrNotMember
	}
	var s Session
	if err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "deleted_at", Value: nil}}, &s); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Viewer{}, ErrNotMember
		}
		return Viewer{}, err
	}
	var m Member
	if err := r.members.FindOne(ctx, sessionUser(id, userID), &m); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Viewer{}, ErrNotMember
		}
		return Viewer{}, err
	}
	level := roleLevel(m.Role)
	if level == ViewNone || level > minted {
		return Viewer{}, ErrNotMember
	}
	return Viewer{actor: userID, session: id, level: level}, nil
}

// roleLevel is the view level a session role carries.
func roleLevel(role string) ViewLevel {
	switch role {
	case RoleGM, RoleCoGM:
		return ViewGM
	case RolePlayer:
		return ViewPlayer
	case RoleObserver:
		return ViewObserver
	}
	return ViewNone
}
