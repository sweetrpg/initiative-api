package data

import (
	"context"

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
