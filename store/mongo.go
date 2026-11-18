package store

import (
	"context"
	"errors"
	"time"

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

func (m mongoCollection) UpsertOne(ctx context.Context, filter, update bson.D) error {
	_, err := m.c.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return mapErr(err)
}

func (m mongoCollection) DeleteOne(ctx context.Context, filter bson.D) error {
	res, err := m.c.DeleteOne(ctx, filter)
	if err != nil {
		return mapErr(err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
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
		if s.ExpireAfter > 0 {
			o.SetExpireAfterSeconds(int32(s.ExpireAfter / time.Second))
		}
		models = append(models, mongo.IndexModel{Keys: s.Keys, Options: o})
	}
	_, err := m.c.Indexes().CreateMany(ctx, models)
	return mapErr(err)
}

// Watch tails the collection's inserts with a change stream (a replica set is required).
func (m mongoCollection) Watch(ctx context.Context, resume bson.Raw) (Cursor, error) {
	opts := options.ChangeStream()
	if len(resume) > 0 {
		opts.SetResumeAfter(resume)
	}
	pipe := mongo.Pipeline{{{Key: "$match", Value: bson.D{{Key: "operationType", Value: "insert"}}}}}
	cs, err := m.c.Watch(ctx, pipe, opts)
	if err != nil {
		return nil, watchError(err)
	}
	return mongoCursor{cs}, nil
}

type mongoCursor struct{ cs *mongo.ChangeStream }

func (c mongoCursor) Next(ctx context.Context) bool { return c.cs.Next(ctx) }
func (c mongoCursor) Doc() bson.Raw {
	d, _ := c.cs.Current.Lookup("fullDocument").DocumentOK()
	return d
}
func (c mongoCursor) Token() bson.Raw                 { return c.cs.ResumeToken() }
func (c mongoCursor) Err() error                      { return watchError(c.cs.Err()) }
func (c mongoCursor) Close(ctx context.Context) error { return c.cs.Close(ctx) }

// watchError marks the server's history-lost answers (ChangeStreamHistoryLost 286, ChangeStreamFatalError
// 280) as ErrHistoryLost; any other error is transient and keeps its resume token.
func watchError(err error) error {
	var ce mongo.CommandError
	if errors.As(err, &ce) && (ce.HasErrorCode(286) || ce.HasErrorCode(280)) {
		return errors.Join(ErrHistoryLost, err)
	}
	return err
}
