package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/relations"
)

// SessionAuthority is auth-api's session surface (PADR-0040): minting a session ID, ending a
// session, and writing membership tuples. *relations.Client implements it. Every call forwards the
// caller's own bearer (PADR-0037).
type SessionAuthority interface {
	Mint(ctx context.Context, token string) (string, error)
	EndSession(ctx context.Context, token, sessionID string) error
	WriteTuples(ctx context.Context, token, sessionID string, writes, deletes []relations.Tuple) error
}

// ErrSyncPending means a membership change was saved but its OpenFGA tuple could not be written.
// The member is denied until the tuple exists, which is the safe direction; repeating the request
// finishes the write, and the reconciler repairs it within one interval otherwise.
var ErrSyncPending = errors.New("domain: membership saved, tuple sync pending")

type authorityKey struct{}

type authority struct {
	svc   SessionAuthority
	token string
}

// WithAuthority puts auth-api's session surface and the caller's bearer on ctx. Without it every
// session change fails closed.
func WithAuthority(ctx context.Context, svc SessionAuthority, token string) context.Context {
	return context.WithValue(ctx, authorityKey{}, authority{svc, token})
}

func authorityOf(ctx context.Context) (authority, error) {
	a, ok := ctx.Value(authorityKey{}).(authority)
	if !ok || a.svc == nil {
		return authority{}, relations.ErrUnavailable
	}
	return a, nil
}

// memberRoles are the relations the membership endpoint writes; gm is never one.
var memberRoles = []string{data.RoleCoGM, data.RolePlayer, data.RoleObserver}

func tupleOf(user, role string) relations.Tuple {
	return relations.Tuple{User: "user:" + user, Relation: role}
}

// syncGrant writes user's role tuple after the Mongo record exists. It deletes the user's other
// non-gm relations in the same atomic write, so a role change or a retry cannot leave two roles;
// a non-GM actor leaves co_gm tuples alone because only the GM changes those (PADR-0040 section 2).
// A rejected token is returned as such; every other failure is ErrSyncPending.
func (s *Sessions) syncGrant(ctx context.Context, actorRole, sessionID, user, role string) error {
	a, err := authorityOf(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSyncPending, err)
	}
	var deletes []relations.Tuple
	for _, r := range memberRoles {
		if r != role && (r != data.RoleCoGM || actorRole == data.RoleGM) {
			deletes = append(deletes, tupleOf(user, r))
		}
	}
	err = a.svc.WriteTuples(ctx, a.token, sessionID, []relations.Tuple{tupleOf(user, role)}, deletes)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, relations.ErrUnauthorized):
		return err
	}
	slog.WarnContext(ctx, "membership tuple write failed; the reconciler will repair it", "session", sessionID, "user", user, "error", err)
	return fmt.Errorf("%w: %w", ErrSyncPending, err)
}

// syncRevoke deletes user's role tuple after the Mongo record is gone. The change has already taken
// effect (the member lost access when the record went), so a failure is logged and left to the
// reconciler instead of failing the request.
func (s *Sessions) syncRevoke(ctx context.Context, sessionID, user, role string) {
	if !slices.Contains(memberRoles, role) {
		return
	}
	a, err := authorityOf(ctx)
	if err == nil {
		err = a.svc.WriteTuples(ctx, a.token, sessionID, nil, []relations.Tuple{tupleOf(user, role)})
	}
	if err != nil {
		slog.WarnContext(ctx, "membership tuple delete failed; the reconciler will remove it", "session", sessionID, "user", user, "error", err)
	}
}

// endSession tells auth-api to drop every tuple on the session and close its mint. The Mongo
// records are already gone, so a failure is logged and left to garbage collection.
func (s *Sessions) endSession(ctx context.Context, sessionID string) {
	a, err := authorityOf(ctx)
	if err == nil {
		err = a.svc.EndSession(ctx, a.token, sessionID)
	}
	if err != nil {
		slog.WarnContext(ctx, "ending the session at auth-api failed; garbage collection will close it", "session", sessionID, "error", err)
	}
}

// canManageRole reports whether actorRole may assign or remove role. Only the GM changes a co_gm
// (PADR-0040): a co_gm calling auth-api directly could otherwise strip other co-GMs.
func canManageRole(actorRole, role string) bool {
	return role != data.RoleCoGM || actorRole == data.RoleGM
}
