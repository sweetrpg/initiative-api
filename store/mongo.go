package store

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoDatabase adapts a *mongo.Database to Database.
type MongoDatabase struct{ DB *mongo.Database }

// Collection returns the named collection.
func (m MongoDatabase) Collection(name string) Collection {
	return mongoCollection{c: m.DB.Collection(name)}
}

type mongoCollection struct{ c *mongo.Collection }

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mongo.ErrNoDocuments):
		return ErrNotFound
	case mongo.IsDuplicateKeyError(err):
		return ErrDuplicate
	}
	return err
}

func (m mongoCollection) InsertOne(ctx context.Context, doc any) error {
	_, err := m.c.InsertOne(ctx, doc)
	return mapErr(err)
}

func (m mongoCollection) FindOne(ctx context.Context, filter bson.D, out any) error {
	return mapErr(m.c.FindOne(ctx, filter).Decode(out))
}

func (m mongoCollection) Find(ctx context.Context, filter bson.D, opts FindOptions, out any) error {
	fo := options.Find()
	if len(opts.Sort) > 0 {
		fo.SetSort(opts.Sort)
	}
	if len(opts.Projection) > 0 {
		fo.SetProjection(opts.Projection)
	}
	if opts.Limit > 0 {
		fo.SetLimit(opts.Limit)
	}
	cur, err := m.c.Find(ctx, filter, fo)
	if err != nil {
		return mapErr(err)
	}
	return mapErr(cur.All(ctx, out))
}

func (m mongoCollection) FindOneAndUpdate(ctx context.Context, filter, update bson.D, out any) error {
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	return mapErr(m.c.FindOneAndUpdate(ctx, filter, update, opts).Decode(out))
}

func (m mongoCollection) CountDocuments(ctx context.Context, filter bson.D) (int64, error) {
	n, err := m.c.CountDocuments(ctx, filter)
	return n, mapErr(err)
}

func (m mongoCollection) EnsureIndexes(ctx context.Context, specs []IndexSpec) error {
	models := make([]mongo.IndexModel, 0, len(specs))
	for _, s := range specs {
		o := options.Index().SetName(s.Name).SetUnique(s.Unique)
		if len(s.Partial) > 0 {
			o.SetPartialFilterExpression(s.Partial)
		}
		models = append(models, mongo.IndexModel{Keys: s.Keys, Options: o})
	}
	_, err := m.c.Indexes().CreateMany(ctx, models)
	return mapErr(err)
}
