package data

import (
	"context"
	"errors"
	"time"

	"github.com/sweetrpg/initiative-api/store"
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

// systemActor attributes the reconciler's own writes (PADR-0001's sentinel for non-user writes).
const systemActor = "system"

// GMlessSessions lists the live sessions created before createdBefore that have no live GM member
// record, so none of them can be deleted by anyone (PADR-0040 section 1, a failed GM insert whose
// rollback also failed). The age bound keeps it away from a session still being created.
// Reconciler only: it has no acting user and reads every session.
func (r *Sessions) GMlessSessions(ctx context.Context, createdBefore time.Time) ([]string, error) {
	base := bson.D{{Key: "deleted_at", Value: nil}, {Key: "created_at", Value: bson.D{{Key: "$lte", Value: createdBefore}}}}
	var out []string
	cursor := ""
	for {
		page, err := runQuery[Session](ctx, r.c, SessionSpec, base,
			Query{Sort: []SortKey{{Field: "created_at"}}, Limit: MaxLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if len(page.Items) > 0 {
			ids := make(bson.A, 0, len(page.Items))
			for _, s := range page.Items {
				ids = append(ids, s.ID)
			}
			var gms []Member
			if err := r.members.Find(ctx, bson.D{
				{Key: "session_id", Value: bson.D{{Key: "$in", Value: ids}}},
				{Key: "role", Value: RoleGM}, {Key: "deleted_at", Value: nil},
			}, store.FindOptions{}, &gms); err != nil {
				return nil, err
			}
			has := make(map[string]bool, len(gms))
			for _, m := range gms {
				has[m.SessionID] = true
			}
			for _, s := range page.Items {
				if !has[s.ID] {
					out = append(out, s.ID)
				}
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// ReapGMlessSession soft-deletes a live session that still has no live GM member record, and every
// record under it, and reports whether it did. It re-reads both facts first, so a session whose GM
// record appeared since GMlessSessions listed it is left alone. A revision that moved is an error
// for the next pass to retry. Reconciler only.
func (r *Sessions) ReapGMlessSession(ctx context.Context, id string) (bool, error) {
	id, ok := cleanID(id)
	if !ok {
		return false, ErrNotFound
	}
	var s Session
	err := r.c.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "deleted_at", Value: nil}}, &s)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var gms []Member
	if err := r.members.Find(ctx, bson.D{
		{Key: "session_id", Value: id}, {Key: "role", Value: RoleGM}, {Key: "deleted_at", Value: nil},
	}, store.FindOptions{Limit: 1}, &gms); err != nil {
		return false, err
	}
	if len(gms) > 0 {
		return false, nil
	}
	if err := r.owned.SoftDelete(ctx, s.OwnerUserID, systemActor, id, s.Revision); err != nil {
		return false, err
	}
	return true, r.dropRecords(ctx, id, systemActor)
}
