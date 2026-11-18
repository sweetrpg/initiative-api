package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sweetrpg/common.go/logging"
	"github.com/sweetrpg/mongodb.go/database"
)

// Connection retry: the cluster resolver sometimes times out on the Atlas SRV lookup right as a
// CronJob pod starts, so a connect or resolve failure is retried with a growing delay for up to
// dbConnectBudget before the pass gives up. An authentication failure will not fix itself, so it
// gets dbAuthAttempts tries in all.
const (
	dbConnectBudget = 60 * time.Second
	dbPingTimeout   = 10 * time.Second
	dbFirstBackoff  = time.Second
	dbMaxBackoff    = 10 * time.Second
	dbAuthAttempts  = 2
)

// dbConn is what connectWithRetry needs from the database package, so a test can fail it on demand.
type dbConn struct {
	setup    func() error // opens the client; an SRV or URI failure is an error
	ping     func(ctx context.Context) error
	teardown func()
	sleep    func(time.Duration)
}

func liveDB() dbConn {
	return dbConn{
		setup: func() (err error) {
			// SetupDatabase panics on a connect or resolve failure.
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			database.SetupDatabase()
			return nil
		},
		ping:     func(ctx context.Context) error { return database.Db.Client().Ping(ctx, nil) },
		teardown: database.TeardownDatabase,
		sleep:    time.Sleep,
	}
}

// connectWithRetry opens the database and checks it answers (the driver connects lazily, so a
// pass would otherwise fail on its first query), retrying as described above. Each failed attempt
// is logged at warn; the last error is returned once the budget is spent.
func connectWithRetry(c dbConn, budget time.Duration) error {
	var waited time.Duration
	backoff := dbFirstBackoff
	var authFails int
	for attempt := 1; ; attempt++ {
		err := c.setup()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
			err = c.ping(ctx)
			cancel()
			if err != nil {
				c.teardown()
			}
		}
		if err == nil {
			return nil
		}
		if isAuthError(err) {
			if authFails++; authFails >= dbAuthAttempts {
				return fmt.Errorf("database authentication failed after %d tries: %w", authFails, err)
			}
		}
		if waited+backoff > budget {
			return fmt.Errorf("database not reachable after %d tries over %s: %w", attempt, waited, err)
		}
		logging.Logger.Warn("database connection failed; retrying", "attempt", attempt, "retry_in", backoff.String(), "error", err.Error())
		c.sleep(backoff)
		waited += backoff
		backoff = min(backoff*2, dbMaxBackoff)
	}
}

func isAuthError(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "authenticationfailed") || strings.Contains(m, "authentication failed") || strings.Contains(m, "auth error")
}
