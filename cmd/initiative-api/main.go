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
	"github.com/joho/godotenv"
	sloggin "github.com/samber/slog-gin"
	"github.com/sweetrpg/api-core.go/tracing"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/common.go/logging"
	"github.com/sweetrpg/initiative-api/config"
	"github.com/sweetrpg/initiative-api/constants"
	"github.com/sweetrpg/initiative-api/server"
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

	srv := &http.Server{Addr: cfg.BindAddress, Handler: newRouter(cfg, authz.NewClient(cfg.AuthAPIURL, cfg.UsersAPIURL)), ReadHeaderTimeout: 10 * time.Second}

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

func newRouter(cfg config.Config, authzClient *authz.Client) *gin.Engine {
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

	server.SetupHandlers(r, authzClient)
	return r
}
