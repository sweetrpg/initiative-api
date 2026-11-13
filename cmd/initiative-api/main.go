// Command initiative-api serves the Initiative! API.
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/joho/godotenv"
	sloggin "github.com/samber/slog-gin"
	"github.com/sweetrpg/api-core.go/ratelimit"
	"github.com/sweetrpg/api-core.go/tracing"
	"github.com/sweetrpg/api-core.go/util"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/common.go/logging"
	"github.com/sweetrpg/initiative-api/config"
	"github.com/sweetrpg/initiative-api/constants"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/events"
	"github.com/sweetrpg/initiative-api/server"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/mongodb.go/database"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

func main() {
	_ = godotenv.Load(".env")

	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("invalid configuration:\n%v", err)
	}

	// common.go/logging reads LOG_LEVEL itself; pass the validated, defaulted value.
	_ = os.Setenv(constants.LOG_LEVEL, cfg.LogLevel)
	logging.Init()

	tracing.SetupTracing(constants.ServiceName)
	defer tracing.TeardownTracing()

	database.SetupDatabase() // reads the MongoDB connection settings from the environment
	defer database.TeardownDatabase()
	db := store.MongoDatabase{DB: database.Db}

	idxCtx, idxCancel := context.WithTimeout(context.Background(), time.Minute)
	if err := data.EnsureIndexes(idxCtx, db); err != nil {
		log.Fatalf("indexes: %v", err)
	}
	idxCancel()

	themes, err := domain.NewThemes(db)
	if err != nil {
		log.Fatalf("themes: %v", err)
	}
	creds := domain.NewCredentials(db)
	// Credentials stay off (503) until the auth-event consumer is bound and caught up, so a deny
	// published while the service was down is applied before any secret authenticates (PADR-0039).
	if natsURL := os.Getenv("NATS_URL"); natsURL == "" {
		logging.Logger.Warn("NATS_URL is not set; integration credentials are disabled")
	} else {
		consumer := events.New(creds, creds)
		if err := consumer.Start(context.Background(), natsURL, os.Getenv("NATS_DURABLE_NAME")); err != nil {
			log.Fatalf("auth events: %v", err)
		}
		defer consumer.Stop()
	}
	// The limiter fails closed without its counter store (PADR-0036), so Redis is required.
	pool := util.RedisPool()
	if pool == nil {
		log.Fatal("REDIS_HOST is required for rate limiting")
	}
	rosters, encounters, sessions := domain.NewRosters(db), domain.NewEncounters(db), domain.NewSessions(db)
	svc := server.Services{
		Locks: encounters.Locks(),
		Limits: domain.NewLimits(
			domain.Kind{Name: "themes", Key: domain.KeyThemeSlots, Repo: themes.Items()},
			domain.Kind{Name: "rosters", Key: domain.KeySavedRosters, Repo: rosters.Items()},
			domain.Kind{Name: "encounters", Key: domain.KeySavedEncounters, Repo: encounters.Items()},
			domain.Kind{Name: "sessions", Key: domain.KeySessionsPerGM, Repo: sessions.Items()},
			domain.Kind{Name: "credentials", Key: domain.KeyIntegrationCredentials, Repo: creds.Items()},
		),
		Authz:        authz.NewClient(cfg.AuthAPIURL, cfg.UsersAPIURL),
		Entitlements: entitlements.New(cfg.EntitlementsAPIURL),
		Themes:       themes,
		Credentials:  creds,
		Rosters:      rosters,
		Encounters:   encounters,
		Sessions:     sessions,
	}
	srv := &http.Server{Addr: cfg.BindAddress, Handler: newRouter(cfg, svc, pool), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Logger.Error("server failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logging.Logger.Error("shutdown failed", "error", err.Error())
	}
}

func newRouter(cfg config.Config, svc server.Services, pool *redis.Pool) *gin.Engine {
	httpLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	r := gin.New()
	// Correlation runs first so the access log and every handler see the ID.
	r.Use(server.Correlation())
	r.Use(sloggin.New(httpLogger))
	r.Use(gin.Recovery())
	r.Use(otelgin.Middleware(constants.ServiceName))

	if len(cfg.AllowedOrigins) > 0 {
		r.Use(cors.New(cors.Config{
			AllowOrigins:     cfg.AllowedOrigins,
			AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", constants.CorrelationHeader},
			ExposeHeaders:    []string{constants.CorrelationHeader},
			AllowCredentials: true,
			MaxAge:           12 * time.Hour,
		}))
	}

	r.Use(server.RateLimit(pool, ratelimit.DefaultOptions())...)

	server.SetupHandlers(r, svc)
	return r
}
