// Package store is the narrow MongoDB surface the data layer needs. The data layer depends on
// this interface so its owner-scoping, revision, and query logic can be tested against an
// in-memory fake (store/fake) without a database.
package store

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
)

// Sentinel errors returned by every Collection implementation.
var (
	// ErrNotFound means no document matched the filter.
	ErrNotFound = errors.New("store: not found")
	// ErrDuplicate means a unique index rejected the write.
	ErrDuplicate = errors.New("store: duplicate key")
)

// FindOptions carries the query-layer sort, projection, and limit for Find.
type FindOptions struct {
	Sort       bson.D
	Projection bson.D
	Limit      int64
}

// IndexSpec describes one index. Partial is a partialFilterExpression; empty means a full index.
type IndexSpec struct {
	Name    string
	Keys    bson.D
	Unique  bool
	Partial bson.D
}

// Collection is one MongoDB collection. Filters and updates are bson documents built from
// parameters; callers never concatenate strings into them. out arguments are pointers decoded
// from the stored document (Find takes a pointer to a slice).
type Collection interface {
	InsertOne(ctx context.Context, doc any) error
	FindOne(ctx context.Context, filter bson.D, out any) error
	Find(ctx context.Context, filter bson.D, opts FindOptions, out any) error
	// FindOneAndUpdate applies update to the first match and decodes the post-update document.
	FindOneAndUpdate(ctx context.Context, filter, update bson.D, out any) error
	CountDocuments(ctx context.Context, filter bson.D) (int64, error)
	EnsureIndexes(ctx context.Context, specs []IndexSpec) error
}

// Database hands out collections by name.
type Database interface {
	Collection(name string) Collection
}
