// Command seed applies the checked-in game-system definitions to the configured database. It is
// idempotent and safe to run at every deploy.
package main

import (
	"context"
	"log"
	"time"

	"github.com/sweetrpg/common.go/logging"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/seed"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/mongodb.go/database"
)

func main() {
	logging.Init()
	database.SetupDatabase() // reads the MongoDB connection settings from the environment
	defer database.TeardownDatabase()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := store.MongoDatabase{DB: database.Db}
	if err := data.EnsureIndexes(ctx, db); err != nil {
		log.Fatalf("indexes: %v", err)
	}
	res, err := seed.Load(ctx, data.NewSystemRepo(db))
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Printf("game systems seeded: created=%d updated=%d unchanged=%d", res.Created, res.Updated, res.Unchanged)
}
