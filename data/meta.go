// Package data is initiative-api's data layer. Every collection that stores user data goes
// through Owned, which scopes each read and write by the caller's user ID (docs/coding-
// conventions.md, "Owner-Scoped Data Access") and pushes filter, sort, projection, and limit
// into the query ("Push Filtering, Sorting, and Counting to the Query Layer").
package data

import (
	"time"

	"github.com/sweetrpg/model-core.go/models"
)

// Meta is embedded (inline) in every owner-scoped document: identity, owner, revision, and the
// platform audit fields (PADR-0001).
type Meta struct {
	ID          string `bson:"_id" json:"id"`
	OwnerUserID string `bson:"owner_user_id" json:"owner_user_id"`
	// Revision starts at 1 and advances on every update; writes compare-and-set on it.
	Revision int64 `bson:"revision" json:"revision"`
	// InactiveAt is set while the record is over the owner's limit: readable and deletable, not
	// usable. KeepRank is the owner's choice order (1 first); zero means no choice was made.
	InactiveAt *time.Time `bson:"inactive_at,omitempty" json:"inactive_at,omitempty"`
	KeepRank   int        `bson:"keep_rank,omitempty" json:"-"`
	// LockedAt is set while the record uses a gated game system its owner is not entitled to.
	LockedAt         *time.Time `bson:"locked_at,omitempty" json:"locked_at,omitempty"`
	models.Auditable `bson:",inline"`
}

// GetMeta lets generic code reach the embedded Meta through the document pointer.
func (m *Meta) GetMeta() *Meta { return m }
