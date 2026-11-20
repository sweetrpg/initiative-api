package domain

import (
	"context"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/store"
)

// NotificationPrefs lets a session member mute notice types for that session.
type NotificationPrefs struct {
	repo     *data.NotificationPrefs
	sessions *Sessions
}

// NewNotificationPrefs builds the service on db.
func NewNotificationPrefs(db store.Database, sessions *Sessions) *NotificationPrefs {
	return &NotificationPrefs{repo: data.NewNotificationPrefs(db), sessions: sessions}
}

// Get returns the types actor has muted for the session. Only a member may read them.
func (s *NotificationPrefs) Get(ctx context.Context, actor, sessionID string) ([]string, error) {
	if _, _, _, err := s.sessions.as(ctx, actor, sessionID, ActView); err != nil {
		return nil, err
	}
	return s.repo.Muted(ctx, actor, sessionID)
}

// Set replaces the types actor has muted for the session and returns the stored list.
func (s *NotificationPrefs) Set(ctx context.Context, actor, sessionID string, muted []string) ([]string, error) {
	if _, _, _, err := s.sessions.as(ctx, actor, sessionID, ActView); err != nil {
		return nil, err
	}
	return s.repo.SetMuted(ctx, actor, sessionID, muted)
}

// Muted is the event path's read of a user's mutes (stream.Preferences): the fan-out acts for the
// addressee, so it reads that one user's record and nobody else's.
func (s *NotificationPrefs) Muted(ctx context.Context, userID, sessionID string) ([]string, error) {
	return s.repo.Muted(ctx, userID, sessionID)
}
