package data

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"go.mongodb.org/mongo-driver/bson"
)

// CollectionNotificationPrefs holds each user's muted notice types per session.
const CollectionNotificationPrefs = "notification_preferences"

// NotificationPrefSpec: one document per user and session, addressed only by the caller's own id.
var NotificationPrefSpec = Spec{
	Name:    CollectionNotificationPrefs,
	Indexes: []store.IndexSpec{{Name: "user_session", Keys: asc("user_id", "session_id"), Unique: true}},
}

type notificationPrefDoc struct {
	UserID    string    `bson:"user_id"`
	SessionID string    `bson:"session_id"`
	Muted     []string  `bson:"muted"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// NotificationPrefs stores which notice types a user has turned off for a session. Every read and
// write is filtered by the owning user's id (docs/coding-conventions.md, owner-scoped data
// access); there is no function that reaches another user's preferences. Whether the user belongs
// to the session is the caller's check.
type NotificationPrefs struct {
	c   store.Collection
	now func() time.Time
}

// NewNotificationPrefs builds the repository on db.
func NewNotificationPrefs(db store.Database) *NotificationPrefs {
	return &NotificationPrefs{c: db.Collection(CollectionNotificationPrefs), now: func() time.Time { return time.Now().UTC() }}
}

// Muted returns the notice types ownerUserID has turned off for sessionID; none when nothing is
// stored.
func (r *NotificationPrefs) Muted(ctx context.Context, ownerUserID, sessionID string) ([]string, error) {
	if err := checkUserID(ownerUserID); err != nil {
		return nil, err
	}
	var doc notificationPrefDoc
	err := r.c.FindOne(ctx, prefScope(ownerUserID, sessionID), &doc)
	if errors.Is(err, store.ErrNotFound) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return doc.Muted, nil
}

// SetMuted replaces ownerUserID's muted types for sessionID in one atomic upsert. Unknown types
// are rejected; duplicates collapse.
func (r *NotificationPrefs) SetMuted(ctx context.Context, ownerUserID, sessionID string, muted []string) ([]string, error) {
	if err := checkUserID(ownerUserID); err != nil {
		return nil, err
	}
	clean := make([]string, 0, len(muted))
	for _, m := range muted {
		if !slices.Contains(NoticeTypes, m) {
			return nil, invalid("%q is not a notification type", m)
		}
		if !slices.Contains(clean, m) {
			clean = append(clean, m)
		}
	}
	slices.Sort(clean)
	set := bson.D{{Key: "muted", Value: clean}, {Key: "updated_at", Value: r.now()}}
	if err := r.c.UpsertOne(ctx, prefScope(ownerUserID, sessionID), bson.D{{Key: "$set", Value: set}}); err != nil {
		return nil, err
	}
	return clean, nil
}

func prefScope(ownerUserID, sessionID string) bson.D {
	return bson.D{{Key: "user_id", Value: ownerUserID}, {Key: "session_id", Value: sessionID}}
}
