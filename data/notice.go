package data

import (
	"context"
	"time"
)

// Notice types: the events addressed to particular users rather than to everyone watching an
// encounter (initiative-notifications).
const (
	NoticeTurnYours       = "turn.yours"
	NoticeTurnNext        = "turn.next"
	NoticeChangeSubmitted = "change_request.submitted"
	NoticeChangeApproved  = "change_request.approved"
	NoticeChangeRejected  = "change_request.rejected"
)

// NoticeTypes lists every type a user can mute for a session.
var NoticeTypes = []string{NoticeTurnYours, NoticeTurnNext, NoticeChangeSubmitted, NoticeChangeApproved, NoticeChangeRejected}

// Notice is one event addressed to Users within a session. Data carries only what any addressee
// may see: ids and names the addressee already controls or manages, never a revision, view tag, or
// GM-only field.
type Notice struct {
	Type        string
	SessionID   string
	EncounterID string
	Users       []string
	Data        map[string]any
	At          time.Time
}

// Notifier receives addressed events. Notify must not block.
type Notifier interface {
	Notify(ctx context.Context, n Notice)
}
