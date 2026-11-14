// Command reconciler runs one OpenFGA tuple reconciliation pass and exits (PADR-0035, PADR-0038).
// It authenticates to auth-api as an Auth0 machine-to-machine client and caches the token in Redis
// (PADR-0041). Exit codes: 1 pass failed or Auth0/Redis failed, 2 missing configuration, 3 auth-api
// rejected a freshly fetched token. Any non-zero exit shows on the CronJob and fires the alert.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/sweetrpg/api-core.go/util"
	"github.com/sweetrpg/common.go/logging"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/reconcile"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/mongodb.go/database"
)

func main() { os.Exit(run()) }

func run() int {
	logging.Init()
	tokens, authURL, ok := tokenSource()
	if !ok {
		return 2
	}

	database.SetupDatabase()
	defer database.TeardownDatabase()

	// Under the CronJob's activeDeadlineSeconds (270); the last minute is reserved for mint
	// garbage collection (reconcile.Reconciler.GCReserve).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sessions := data.NewSessions(store.MongoDatabase{DB: database.Db})
	api := reconcile.NewAuthAPIWithSource(authURL, tokens)
	r := reconcile.Reconciler{Members: sessions, Tuples: api, Mints: api, Sessions: sessions}
	res, err := r.Run(ctx)
	logging.Logger.Info("reconcile finished", "sessions", res.Sessions, "written", res.Written, "deleted", res.Deleted, "failed", res.Failed,
		"mints_closed", res.MintsClosed, "mints_skipped", res.MintsSkipped)
	switch {
	case errors.Is(err, reconcile.ErrTokenRejected):
		logging.Logger.Error("auth-api rejected a freshly fetched reconciler token; check the initiative-reconciler role, deny entries and login_profiles mapping (PADR-0041)")
		return 3
	case err != nil:
		logging.Logger.Error("reconcile failed", "error", err.Error())
		return 1
	}
	return 0
}

// tokenSource builds the Auth0 client-credentials source from the environment. The client secret
// is read here and never logged.
func tokenSource() (src reconcile.TokenSource, authURL string, ok bool) {
	authURL = os.Getenv("AUTH_API_URL")
	id, secret := os.Getenv("RECONCILER_CLIENT_ID"), os.Getenv("RECONCILER_CLIENT_SECRET")
	tokenURL, audience := os.Getenv("RECONCILER_TOKEN_URL"), os.Getenv("RECONCILER_AUDIENCE")
	for name, v := range map[string]string{"AUTH_API_URL": authURL, "RECONCILER_CLIENT_ID": id, "RECONCILER_CLIENT_SECRET": secret,
		"RECONCILER_TOKEN_URL": tokenURL, "RECONCILER_AUDIENCE": audience} {
		if v == "" {
			log.Print(name + " is required")
			return nil, "", false
		}
	}
	pool := util.RedisPool()
	if pool == nil {
		log.Print("REDIS_HOST is required for the token cache")
		return nil, "", false
	}
	return reconcile.NewClientCredentials(tokenURL, id, secret, audience, pool), authURL, true
}
