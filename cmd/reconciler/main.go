// Command reconciler runs one OpenFGA tuple reconciliation pass and exits (PADR-0035, PADR-0038).
// It exits non-zero when the pass fails or the reconciler token is rejected or expired, so the
// CronJob shows the failure and the alert fires.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sweetrpg/common.go/logging"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/reconcile"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/mongodb.go/database"
)

func main() { os.Exit(run()) }

func run() int {
	logging.Init()
	authURL, tokenFile := os.Getenv("AUTH_API_URL"), os.Getenv("RECONCILER_TOKEN_FILE")
	if authURL == "" || tokenFile == "" {
		log.Print("AUTH_API_URL and RECONCILER_TOKEN_FILE are required")
		return 2
	}
	raw, err := os.ReadFile(tokenFile)
	token := strings.TrimSpace(string(raw))
	if err != nil || token == "" {
		log.Print("reconciler token file is missing or empty; mint a new token (PADR-0038)")
		return 2
	}

	database.SetupDatabase()
	defer database.TeardownDatabase()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	r := reconcile.Reconciler{
		Members: data.NewSessions(store.MongoDatabase{DB: database.Db}),
		Tuples:  reconcile.NewAuthAPI(authURL, token),
	}
	res, err := r.Run(ctx)
	logging.Logger.Info("reconcile finished", "sessions", res.Sessions, "written", res.Written, "deleted", res.Deleted, "failed", res.Failed)
	switch {
	case errors.Is(err, reconcile.ErrTokenRejected):
		logging.Logger.Error("reconciler token rejected or expired; an operator must mint a new one")
		return 3
	case err != nil:
		logging.Logger.Error("reconcile failed", "error", err.Error())
		return 1
	}
	return 0
}
