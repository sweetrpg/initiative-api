package startup

import (
	"context"
	"fmt"
	"time"

	"github.com/sweetrpg/mongodb.go/database"
)

// pingTimeout bounds the check that a freshly opened client reaches the server.
const pingTimeout = 10 * time.Second

// DB is what ConnectDB needs from the database package, so a test can fail it on demand.
type DB struct {
	Setup    func() error // opens the client; an SRV or URI failure is an error
	Ping     func(ctx context.Context) error
	Teardown func()
}

// LiveDB is the real database package.
func LiveDB() DB {
	return DB{
		Setup: func() (err error) {
			// SetupDatabase panics on a connect or resolve failure.
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			database.SetupDatabase()
			return nil
		},
		Ping:     func(ctx context.Context) error { return database.Db.Client().Ping(ctx, nil) },
		Teardown: database.TeardownDatabase,
	}
}

// ConnectDatabase opens the database and checks it answers (the driver connects lazily, so a
// process would otherwise fail on its first query), retrying as p says. A client that opened but
// did not answer is closed before the next try.
func ConnectDatabase(p Policy) error {
	ApplyDriverTimeouts()
	return ConnectDB(LiveDB(), p)
}

// ConnectDB is ConnectDatabase over c.
func ConnectDB(c DB, p Policy) error {
	return Do("connect to the database", p, func() error {
		if err := c.Setup(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			c.Teardown()
			return err
		}
		return nil
	})
}
