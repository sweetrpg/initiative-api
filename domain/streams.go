package domain

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/sweetrpg/initiative-api/constants"
	"time"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
)

// ErrTicketInvalid means a stream ticket is unknown, expired, or already spent. They are one
// answer, so a probe learns nothing about which.
var ErrTicketInvalid = errors.New("stream ticket is not valid")

// DefaultTicketTTL is how long a minted ticket may be redeemed.
const DefaultTicketTTL = constants.DefaultStreamTicketTTL

// Streams issues and redeems the one-time tickets that open an event stream for a browser.
type Streams struct {
	tickets  *data.StreamTickets
	sessions *Sessions
	enc      *Encounters
	// TTL is how long a ticket lives; DefaultTicketTTL when zero.
	TTL time.Duration
}

// NewStreams builds the stream service on db.
func NewStreams(db store.Database, sessions *Sessions, enc *Encounters) *Streams {
	return &Streams{tickets: data.NewStreamTickets(db), sessions: sessions, enc: enc}
}

// SetClock replaces the clock ticket ages are read from (tests).
func (s *Streams) SetClock(now func() time.Time) { s.tickets.SetClock(now) }

func (s *Streams) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultTicketTTL
}

// TicketRequest names what a ticket is for: a session's encounters (SessionID, and optionally one
// of them), or the caller's own private encounters (no SessionID, optionally one of them).
type TicketRequest struct {
	SessionID   string
	EncounterID string
	ClientID    string
}

// Claim is what a redeemed ticket asks for. It is a request, not proof: View decides.
type Claim struct {
	UserID, SessionID, EncounterID, ClientID string
	Level                                    data.ViewLevel
}

// Mint authorizes actor for the scope the way the REST reads do (a non-member is data.ErrNotMember)
// and returns a ticket to open it with, valid for the service's TTL. The caller's own bearer is
// what authorizes (PADR-0037).
func (s *Streams) Mint(ctx context.Context, actor string, req TicketRequest) (string, time.Duration, error) {
	t := data.StreamTicket{UserID: actor, SessionID: req.SessionID, EncounterID: req.EncounterID}
	if data.ValidClientID(req.ClientID) {
		t.ClientID = req.ClientID
	}
	if req.SessionID != "" {
		v, err := s.sessions.Viewer(ctx, actor, req.SessionID)
		if err != nil {
			return "", 0, err
		}
		if req.EncounterID != "" {
			if _, err := s.enc.repo.LoadForViewer(ctx, v, req.EncounterID); err != nil {
				return "", 0, err
			}
		}
		t.Level = v.Level()
		t.SessionID = v.SessionID()
	} else {
		v, err := s.ownerView(ctx, actor, req.EncounterID)
		if err != nil {
			return "", 0, err
		}
		t.Level = v.Level()
	}
	raw := make([]byte, 32)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", 0, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	t.ID = hashTicket(ticket)
	if err := s.tickets.Mint(ctx, t); err != nil {
		return "", 0, err
	}
	return ticket, s.ttl(), nil
}

func hashTicket(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// ownerView is the viewer for the owner's private scope; a named encounter must be theirs and
// private (one in a session is data.ErrForbidden, as on the REST path).
func (s *Streams) ownerView(ctx context.Context, owner, encounterID string) (data.Viewer, error) {
	if encounterID == "" {
		return data.OwnerViewer(owner)
	}
	_, v, err := s.enc.repo.LoadOwnedView(ctx, owner, encounterID)
	return v, err
}

// BearerView is the viewer for a caller authenticated by bearer: the REST authorization (OpenFGA
// through the caller's own token, fail closed) for a session scope, ownership for a private one.
func (s *Streams) BearerView(ctx context.Context, actor, sessionID, encounterID string) (data.Viewer, error) {
	if sessionID == "" {
		return s.ownerView(ctx, actor, encounterID)
	}
	v, err := s.sessions.Viewer(ctx, actor, sessionID)
	if err == nil && encounterID != "" {
		_, err = s.enc.repo.LoadForViewer(ctx, v, encounterID)
	}
	return v, err
}

// ErrAccessChanged means a stream's viewer no longer matches the member's live record.
var ErrAccessChanged = errors.New("stream access changed")

// Recheck re-reads the live member record behind an open stream. It returns ErrAccessChanged when
// the member is gone, the session is deleted, or their level is no longer the one the stream was
// granted at (it must reconnect to be authorized again). A failed read returns nil: the check runs
// again on the next heartbeat, and the connection's maximum age bounds a persistent failure.
func (s *Streams) Recheck(ctx context.Context, v data.Viewer) error {
	if v.SessionID() == "" {
		return nil
	}
	now, err := s.sessions.repo.TicketView(ctx, v.Actor(), v.SessionID(), v.Level())
	switch {
	case errors.Is(err, data.ErrNotMember):
		return ErrAccessChanged
	case err != nil:
		return nil
	case now.Level() != v.Level():
		return ErrAccessChanged
	}
	return nil
}

// Redeem spends a ticket. It is the only thing that can be done with it: a second redeem, an
// expired ticket and an unknown one are all ErrTicketInvalid.
func (s *Streams) Redeem(ctx context.Context, ticket string) (Claim, error) {
	if len(ticket) == 0 || len(ticket) > 128 {
		return Claim{}, ErrTicketInvalid
	}
	t, err := s.tickets.Redeem(ctx, hashTicket(ticket), s.ttl())
	if errors.Is(err, data.ErrNotFound) {
		return Claim{}, ErrTicketInvalid
	}
	if err != nil {
		return Claim{}, err
	}
	return Claim{UserID: t.UserID, SessionID: t.SessionID, EncounterID: t.EncounterID, ClientID: t.ClientID, Level: t.Level}, nil
}

// View is the viewer a redeemed claim opens. Only the user, the scope and the minted level come
// from the ticket; access is read live (data.Sessions.TicketView), so a member removed since the
// mint, a session deleted since, or a role raised since gets nothing.
func (s *Streams) View(ctx context.Context, c Claim) (data.Viewer, error) {
	if c.SessionID == "" {
		return s.ownerView(ctx, c.UserID, c.EncounterID)
	}
	v, err := s.sessions.repo.TicketView(ctx, c.UserID, c.SessionID, c.Level)
	if err != nil {
		return data.Viewer{}, err
	}
	if c.EncounterID != "" {
		if _, err := s.enc.repo.LoadForViewer(ctx, v, c.EncounterID); err != nil {
			return data.Viewer{}, err
		}
	}
	return v, nil
}

// Head is the cheapest thing a client can compare with its cache to learn whether it is current:
// the revision for a GM or co-GM, the view tag for everyone else. Nothing else about the encounter.
type Head struct {
	EncounterID string `json:"encounter_id"`
	Revision    int64  `json:"revision,omitempty"`
	ViewTag     string `json:"view_tag,omitempty"`
}

// MaxHeads is the most heads a stream opens with; more is reported as truncated in hello, and the
// client lists the rest over REST.
const MaxHeads = 100

// Heads returns the head of each encounter in v's scope (or of encounterID only), built from the
// same views the REST reads return.
func (s *Streams) Heads(ctx context.Context, v data.Viewer, encounterID string) ([]Head, bool, error) {
	var views []EncounterView
	var err error
	switch {
	case v.SessionID() != "" && encounterID != "":
		var one EncounterView
		if one, err = s.enc.ViewForSession(ctx, v, encounterID); err == nil {
			views = []EncounterView{one}
		}
	case v.SessionID() != "":
		views, err = s.enc.ViewsForSession(ctx, v, MaxHeads+1)
	case encounterID != "":
		var one EncounterView
		if one, err = s.enc.ViewOwned(ctx, v.Actor(), encounterID); err == nil {
			views = []EncounterView{one}
		}
	default:
		var page data.Page[EncounterView]
		if page, err = s.enc.ViewsOwned(ctx, v.Actor(), data.Query{Limit: MaxHeads + 1}); err == nil {
			views = page.Items
		}
	}
	if err != nil {
		return nil, false, err
	}
	truncated := len(views) > MaxHeads
	if truncated {
		views = views[:MaxHeads]
	}
	out := make([]Head, 0, len(views))
	for _, view := range views {
		h := Head{}
		h.EncounterID, _ = view["id"].(string)
		h.Revision, _ = view["revision"].(int64)
		h.ViewTag, _ = view[FieldViewTag].(string)
		out = append(out, h)
	}
	return out, truncated, nil
}
