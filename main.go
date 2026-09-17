package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Daniel-dev22/agent-kit-go/logging"
	"github.com/gin-gonic/gin"
)

func main() {
	logging.Setup(os.Getenv("LOG_LEVEL"))
	attachRotatingLogFile(os.Getenv("LOG_LEVEL"))

	cfg := loadConfig()
	slog.Info("docker-agent starting", "node", cfg.NodeName, "site", cfg.SiteID, "docker_host", cfg.DockerHost)

	reportBootGap(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app, err := newApp(ctx, cfg)
	if err != nil {
		slog.Error("failed to initialize app", "error", err)
		os.Exit(1)
	}
	defer app.close()
	slog.Info("docker engine connected", "version", app.docker.serverVersion)

	go heartbeatLoop(ctx, cfg, 30*time.Second)

	// Recover jobs stuck running/pending from a prior exit BEFORE serving.
	sweepOrphanJobs(ctx, cfg, app.events)
	app.sweepIdempotency(ctx)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(httpLogMiddleware())
	registerRoutes(r, app)

	srv := &http.Server{Addr: ":8080", Handler: r}
	go func() {
		slog.Info("HTTP server listening on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	app.startBackgroundWorkers(ctx)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP shutdown error", "error", err)
	}
}

func registerRoutes(r *gin.Engine, app *app) {
	// Every request body is bounded before any route reads it (bodylimit.go).
	r.Use(boundedBody())

	r.GET("/health/live", app.handleLiveness)
	r.GET("/health/ready", app.handleReadiness)

	// Job introspection — every mutation below is tracked as a Job.
	r.GET("/v1/jobs", app.handleListJobs)
	r.GET("/v1/jobs/:id", app.handleGetJob)
	r.GET("/v1/jobs/:id/log", app.handleGetJobLog)
	r.POST("/v1/jobs/:id/cancel", app.handleCancelJob)

	// Container lifecycle mutations — short async jobs (202 + job id). Every route
	// that can start a job honours an Idempotency-Key header (idempotency.go).
	idem := app.idempotent()
	r.POST("/v1/containers/:id/start", idem, app.handleContainerStart)
	r.POST("/v1/containers/:id/stop", idem, app.handleContainerStop)
	r.POST("/v1/containers/:id/restart", idem, app.handleContainerRestart)
	r.DELETE("/v1/containers/:id", idem, app.handleContainerRemove)
	r.POST("/v1/containers/bulk", idem, app.handleContainerBulk)
	// Historical (non-follow) log window — the viewer's back-paging walks
	// backwards via ?until=<oldest-ts>; the live tail is the WS route below.
	r.GET("/v1/containers/:id/logs", app.handleContainerLogsHistory)

	// Compose project ops + registry — ops are long async jobs (202 + job id);
	// CRUD/copy/bundle are synchronous. Project name is the path param.
	r.GET("/v1/projects", app.handleListProjects)
	r.POST("/v1/projects", idem, app.handleRegisterProject)
	r.DELETE("/v1/projects/:name", app.handleDeregisterProject)
	r.POST("/v1/projects/:name/op", idem, app.handleComposeOp)
	r.GET("/v1/projects/:name/bundle", app.handleProjectBundle)
	r.POST("/v1/projects/:name/copy", idem, app.handleCopyProject)

	// Image-outdated detection — raw cache + manual refresh; the live status
	// normally rides the /ws/fleet snapshot.
	r.GET("/v1/images/checks", app.handleImageChecks)
	r.POST("/v1/images/refresh", app.handleImageCheckRefresh)

	r.GET("/ws/jobs/:id/logs", app.handleJobLogsWS)
	r.GET("/ws/containers/:id/logs", app.handleContainerLogsWS)
	r.GET("/ws/fleet", app.handleFleetWS)
}

func httpLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("http", "method", c.Request.Method, "path", c.Request.URL.Path,
			"status", c.Writer.Status(), "dur", time.Since(start))
	}
}
