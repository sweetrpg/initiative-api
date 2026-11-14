package data

import (
	"context"
	"errors"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/model-core.go/models"
	"go.mongodb.org/mongo-driver/bson"
)

// ScanMembers pages through every membership record, live and soft-deleted, across all sessions.
// It exists for the OpenFGA reconciler (PADR-0035, PADR-0038), which has no acting user and must
// also see deleted records so it can remove their tuples. It is not reachable from a request.
func (r *Sessions) ScanMembers(ctx context.Context, q Query) (Page[Member], error) {
	if len(q.Sort) == 0 {
		q.Sort = []SortKey{{Field: "created_at"}}
	}
	return runQuery[Member](ctx, r.members, SessionMemberSpec, bson.D{}, q)
}

// SessionMembers returns every membership record of one session, live and soft-deleted, read
// fresh. Reconciler only: it has no acting user (PADR-0035, PADR-0038).
func (r *Sessions) SessionMembers(ctx context.Context, sessionID string) ([]Member, error) {
	id, ok := cleanID(sessionID)
	if !ok {
		return nil, ErrNotFound
	}
	var out []Member
	cursor := ""
	for {
		page, err := runQuery[Member](ctx, r.members, SessionMemberSpec, bson.D{{Key: "session_id", Value: id}},
			Query{Sort: []SortKey{{Field: "created_at"}}, Limit: MaxLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// LookupSessions maps each of ids that has a session record to whether it is soft-deleted; an ID
// with no record is absent. It reads no session content. Reconciler only: it decides which
// minted session IDs never became sessions (PADR-0040).
func (r *Sessions) LookupSessions(ctx context.Context, ids []string) (map[string]bool, error) {
	var found []Session
	in := make(bson.A, 0, len(ids))
	for _, id := range ids {
		in = append(in, id)
	}
	if err := r.c.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: in}}}}, store.FindOptions{}, &found); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(found))
	for _, s := range found {
		out[s.ID] = s.DeletedAt != nil
	}
	return out, nil
}

// SessionsWithGM returns the IDs among ids that have a live GM member record. Reconciler only.
func (r *Sessions) SessionsWithGM(ctx context.Context, ids []string) (map[string]bool, error) {
	in := make(bson.A, 0, len(ids))
	for _, id := range ids {
		in = append(in, id)
	}
	var gms []Member
	filter := bson.D{{Key: "session_id", Value: bson.D{{Key: "$in", Value: in}}}, {Key: "role", Value: RoleGM}, {Key: "deleted_at", Value: nil}}
	if err := r.members.Find(ctx, filter, store.FindOptions{}, &gms); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(gms))
	for _, m := range gms {
		out[m.SessionID] = true
	}
	return out, nil
}

// RemoveGMLess soft-deletes a live session that has no live GM record, and every membership record
// of it, as the system. Such a session is left by a creation whose GM record could not be stored
// and whose removal also failed (Open); nobody can delete it, and it counts toward the owner's
// sessions_per_gm. It refuses (ErrConflict) a session that does have a GM, so a lookup that raced
// a creation cannot remove a real session. Reconciler only.
func (r *Sessions) RemoveGMLess(ctx context.Context, sessionID string) error {
	id, ok := cleanID(sessionID)
	if !ok {
		return ErrNotFound
	}
	var s Session
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "deleted_at", Value: nil}}, &s)
	if errors.Is(err, store.ErrNotFound) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	var gm Member
	switch err := r.members.FindOne(ctx, bson.D{{Key: "session_id", Value: id}, {Key: "role", Value: RoleGM}, {Key: "deleted_at", Value: nil}}, &gm); {
	case err == nil:
		return ErrConflict
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	if err := r.owned.SoftDelete(ctx, s.OwnerUserID, models.SystemActor, id, s.Revision); err != nil {
		return err
	}
	return r.dropRecords(ctx, id, models.SystemActor)
}
