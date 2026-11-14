// Command reconciler runs one OpenFGA tuple reconciliation pass and exits (PADR-0035, PADR-0038).
// It authenticates to auth-api as an Auth0 machine-to-machine client and caches the token in Redis
// (PADR-0041). Exit codes: 1 pass failed or Auth0/Redis failed, 2 missing configuration, 3 auth-api
// rejected the token. Any non-zero exit shows on the CronJob and fires the alert.
package main

import (
	"context"
	"errors"
	"log"
	"net/url"
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
		logging.Logger.Error("auth-api rejected the reconciler token (401, 403, or a rejection recorded in the last hour); check the initiative-reconciler role, deny entries, login_profiles mapping and audience (PADR-0041)")
		return 3
	case err != nil:
		logging.Logger.Error("reconcile failed", "error", err.Error())
		return 1
	}
	return 0
}

// settings is the reconciler's token configuration.
type settings struct {
	authURL, clientID, clientSecret, tokenURL, audience string
}

// loadSettings reads and validates the environment. The token URL must be https so the client
// secret never crosses the network in clear text. The secret is read here and never logged.
func loadSettings(get func(string) string) (settings, error) {
	s := settings{
		authURL: get("AUTH_API_URL"), clientID: get("RECONCILER_CLIENT_ID"), clientSecret: get("RECONCILER_CLIENT_SECRET"),
		tokenURL: get("RECONCILER_TOKEN_URL"), audience: get("RECONCILER_AUDIENCE"),
	}
	for _, f := range []struct{ name, value string }{
		{"AUTH_API_URL", s.authURL}, {"RECONCILER_CLIENT_ID", s.clientID}, {"RECONCILER_CLIENT_SECRET", s.clientSecret},
		{"RECONCILER_TOKEN_URL", s.tokenURL}, {"RECONCILER_AUDIENCE", s.audience}, {"REDIS_HOST", get("REDIS_HOST")},
	} {
		if f.value == "" {
			return settings{}, errors.New(f.name + " is required")
		}
	}
	if u, err := url.Parse(s.tokenURL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return settings{}, errors.New("RECONCILER_TOKEN_URL must be an https URL without credentials")
	}
	return s, nil
}

// tokenSource builds the Auth0 client-credentials source from the environment.
func tokenSource() (src reconcile.TokenSource, authURL string, ok bool) {
	s, err := loadSettings(os.Getenv)
	if err != nil {
		log.Print(err.Error())
		return nil, "", false
	}
	pool := util.RedisPool()
	if pool == nil {
		log.Print("REDIS_HOST is required for the token cache")
		return nil, "", false
	}
	return reconcile.NewClientCredentials(s.tokenURL, s.clientID, s.clientSecret, s.audience, pool), s.authURL, true
}
