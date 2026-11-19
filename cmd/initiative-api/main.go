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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/joho/godotenv"
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
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/relations"
	"github.com/sweetrpg/initiative-api/server"
	"github.com/sweetrpg/initiative-api/startup"
	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/mongodb.go/database"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

	// Reads the MongoDB connection settings from the environment; waits out a brief resolver or
	// network failure instead of crashing the pod.
	retry := startup.DefaultPolicy()
	if err := startup.ConnectDatabase(retry); err != nil {
		log.Fatalf("database: %v", err)
	}
	defer database.TeardownDatabase()
	db := store.MongoDatabase{DB: database.Db}

	fanout := db.Collection(stream.FanoutCollection)
	if err := startup.Do("create indexes", startup.IndexPolicy(retry), func() error { // a conflicting index never resolves by waiting
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := data.EnsureIndexes(ctx, db); err != nil {
			return err
		}
		return fanout.EnsureIndexes(ctx, stream.FanoutIndexes)
	}); err != nil {
		log.Fatalf("indexes: %v", err)
	}

	themes, err := domain.NewThemes(db)
	if err != nil {
		log.Fatalf("themes: %v", err)
	}
	creds := domain.NewCredentials(db)
	// Credentials stay off (503) until the auth-event consumer is bound and caught up, so a deny
	// published while the service was down is applied before any secret authenticates (PADR-0039).
	// The consumer starts in the background, so the rest of the API does not wait on NATS; if NATS
	// stays unreachable past CREDENTIAL_EVENTS_MAX_DISCONNECT the gate closes again.
	if natsURL := os.Getenv("NATS_URL"); natsURL == "" {
		logging.Logger.Warn("NATS_URL is not set; integration credentials are disabled")
	} else {
		var live atomic.Pointer[events.Consumer]
		consumerCtx, stopConsumer := context.WithCancel(context.Background())
		go runConsumer(consumerCtx, creds, natsURL, os.Getenv("NATS_DURABLE_NAME"), cfg.CredentialEventsMaxDisconnect, retry, &live)
		defer stopConsumer()
		defer func() {
			if c := live.Load(); c != nil {
				c.Stop()
			}
		}()
	}
	pool := util.RedisPool() // non-nil: config requires REDIS_HOST
	rosters, encounters, sessions := domain.NewRosters(db), domain.NewEncounters(db), domain.NewSessions(db)
	hub := stream.New(encounters)
	defer hub.Close()
	encounters.SetSink(hub)
	sessions.SetSink(hub)
	sessions.SetEvictor(hub)
	host, _ := os.Hostname()
	pod := host + "-" + primitive.NewObjectID().Hex() // unique per process: a pod never mistakes another's rows for its own
	relay := hub.AttachRelay(fanout, pod, 1024)
	relay.Run(context.Background())
	defer relay.Stop()
	rosters.SetSink(hub)
	sessions.OwnerRecordMaxAge = cfg.OwnerEntitlementsMaxAge
	sessions.MaxMembers = cfg.SessionMembersMax
	streams := domain.NewStreams(db, sessions, encounters)
	streams.TTL = cfg.StreamTicketTTL
	svc := server.Services{
		Locks: encounters.Locks(),
		Limits: domain.NewLimits(
			domain.Kind{Name: "themes", Key: domain.KeyThemeSlots, Repo: themes.Items()},
			domain.Kind{Name: "rosters", Key: domain.KeySavedRosters, Repo: rosters.Items()},
			domain.Kind{Name: "encounters", Key: domain.KeySavedEncounters, Repo: encounters.Items()},
			domain.Kind{Name: "sessions", Key: domain.KeySessionsPerGM, Repo: sessions.Items()},
			domain.Kind{Name: "credentials", Key: domain.KeyIntegrationCredentials, Repo: creds.Items()},
		),
		Authz:          authz.NewClient(cfg.AuthAPIURL, cfg.UsersAPIURL),
		Entitlements:   entitlements.New(cfg.EntitlementsAPIURL),
		OwnerLimits:    data.NewOwnerEntitlements(db, cfg.OwnerEntitlementsMaxAge),
		Themes:         themes,
		Systems:        data.NewSystemRepo(db),
		Credentials:    creds,
		Rosters:        rosters,
		Encounters:     encounters,
		Sessions:       sessions,
		ChangeRequests: domain.NewChangeRequests(db, encounters, sessions),
		Relations:      relations.New(cfg.AuthAPIURL),
		Hub:            hub,
		Streams:        streams,
		Leases:         data.NewStreamLeases(db),
		Stream: server.StreamOptions{
			AllowedOrigins: cfg.StreamAllowedOrigins, MaxAge: cfg.StreamMaxAge, Heartbeat: cfg.StreamHeartbeat,
			ConcurrentMax: cfg.StreamConcurrentMax, CredentialMax: cfg.StreamCredentialMax, MaxPerPod: cfg.StreamMaxPerPod, LeaseTTL: cfg.StreamLeaseTTL, Grace: cfg.StreamGrace, Pod: pod,
		},
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
	r.Use(server.Middleware(httpLogger)...)

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

	r.Use(server.RateLimit(pool, ratelimit.DefaultOptions(), svc.Credentials)...)

	server.SetupHandlers(r, svc)
	return r
}

// runConsumer keeps an auth-event consumer running until ctx ends: it starts one (retrying with a
// capped backoff, an error logged each round, so a NATS that is down at boot is picked up when it
// returns) and builds a new one if the connection is lost for good. Until a consumer has started and
// caught up, credentials stay closed (503) and the rest of the API serves.
func runConsumer(ctx context.Context, creds *domain.Credentials, natsURL, durable string, maxDisconnect time.Duration, retry startup.Policy, live *atomic.Pointer[events.Consumer]) {
	wait := 5 * time.Second
	for ctx.Err() == nil {
		var c *events.Consumer
		err := startup.Do("start the auth-event consumer", retry, func() error {
			c = events.New(creds, creds)
			c.MaxDisconnect = maxDisconnect
			return c.Start(ctx, natsURL, durable)
		})
		if err != nil {
			logging.Logger.Error("the auth-event consumer did not start; integration credentials stay closed; retrying", "error", err.Error(), "retry_in", wait.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, 2*time.Minute)
			continue
		}
		wait = 5 * time.Second
		live.Store(c)
		select {
		case <-c.Dead():
			logging.Logger.Error("the auth-event consumer lost its connection for good; starting a new one")
			c.Stop()
		case <-ctx.Done():
			return
		}
	}
}
