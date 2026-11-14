package data

import (
	"context"

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
