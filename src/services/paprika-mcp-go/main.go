package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/auth"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/config"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/handler"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mcpserver"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/service"
)

const shutdownTimeout = 10 * time.Second

func main() {
	stdio := flag.Bool("stdio", false, "serve MCP over stdio instead of HTTP")
	flag.Parse()

	// .env must load before the logger is built so LOG_LEVEL can come from it.
	dotenvErr := config.LoadDotenv()

	// stdout carries the MCP protocol in stdio mode, so logs go to stderr there.
	logOut := os.Stdout
	if *stdio {
		logOut = os.Stderr
	}

	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{
		Level: config.LogLevelFromEnv(),
	}))
	slog.SetDefault(logger)

	if dotenvErr != nil {
		logger.Error("could not read .env file", "error", dotenvErr)
		os.Exit(1)
	}

	load := config.Load
	if *stdio {
		load = config.LoadStdio
	}
	cfg, err := load(logger)
	if err != nil {
		logger.Error("could not load configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(ctx, logger, cfg)
	if err != nil {
		logger.Error("could not start", "error", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		logger.Info("paprika-mcp-go shutting down")
	}()

	if *stdio {
		err = app.runStdio(ctx)
	} else {
		err = app.runHTTP(ctx, cfg)
	}
	stop() // also stops the cache when the server failed before a signal
	app.shutdown()
	if err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}

	logger.Info("paprika-mcp-go stopped")
}

// app holds the pieces shared by both transports: the Paprika client, the
// cache, the snapshot saver, and the MCP server built on them.
type app struct {
	logger *slog.Logger
	server *mcp.Server

	cacheDone chan struct{}
	saverDone chan struct{}
	stopSaver context.CancelFunc
}

// newApp builds client -> cache -> service -> MCP server and starts the cache
// loading in the background. ctx stops the cache; the saver stops in shutdown
// so that its final save follows the cache's last change.
func newApp(ctx context.Context, logger *slog.Logger, cfg *config.Config) (*app, error) {
	client, err := paprikaclient.New(paprikaclient.Config{
		Email:             cfg.PaprikaEmail,
		Password:          cfg.PaprikaPassword,
		UserAgent:         "paprika-mcp-go/" + mcpserver.Version,
		Logger:            logger,
		RequestsPerMinute: cfg.RequestsPerMinute,
		RequestBurst:      cfg.RequestBurst,
	})
	if err != nil {
		return nil, fmt.Errorf("could not build Paprika client: %w", err)
	}

	cfg.Index.FormatVersion = caching.SnapshotFormatVersion
	store, err := indexstore.New(ctx, cfg.Index)
	if err != nil {
		return nil, fmt.Errorf("could not set up index snapshot store: %w", err)
	}

	cache := caching.New(client, caching.Options{Logger: logger, Store: store})
	saver := indexstore.NewSaver(store, cache.Snapshot, indexstore.SaverConfig{Logger: logger})
	cache.SetOnChange(saver.MarkDirty)

	svc := service.New(cache, logger, service.WithWriter(client))
	a := &app{
		logger: logger,
		server: mcpserver.New(
			svc,
			mcpserver.Options{
				Logger:        logger,
				WritesEnabled: cfg.WritesEnabled,
				CallTimeout:   mcpserver.DefaultCallTimeout,
			},
		),
		cacheDone: make(chan struct{}),
		saverDone: make(chan struct{}),
	}

	saverCtx, stopSaver := context.WithCancel(context.Background())
	a.stopSaver = stopSaver
	go func() {
		defer close(a.saverDone)
		saver.Run(saverCtx)
	}()
	go func() {
		defer close(a.cacheDone)
		cache.Run(ctx)
	}()

	logger.Info("recipe index loading in the background", "index_store", string(cfg.Index.Mode))
	return a, nil
}

// shutdown waits for the cache to stop, then stops the saver, which makes its
// final save. Each wait is bounded.
func (a *app) shutdown() {
	wait := func(done <-chan struct{}, what string) {
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			a.logger.Warn("timed out waiting for shutdown", "component", what)
		}
	}
	wait(a.cacheDone, "cache")
	a.stopSaver()
	wait(a.saverDone, "index saver")
}

func (a *app) runStdio(ctx context.Context) error {
	if err := a.server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func (a *app) runHTTP(ctx context.Context, cfg *config.Config) error {
	verifier, err := auth.NewVerifier(cfg.TokenSHA256)
	if err != nil {
		return fmt.Errorf("could not build token verifier: %w", err)
	}

	srv := &http.Server{
		Addr: ":" + strconv.Itoa(cfg.Port),
		Handler: handler.NewRouter(handler.Options{
			Logger:         a.logger,
			MCPServer:      a.server,
			Verifier:       verifier.Verify,
			AllowedOrigins: cfg.AllowedOrigins,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout must outlast the request timeout, or a slow request would
		// be cut off with an empty response instead of the timeout fallback.
		WriteTimeout:   handler.DefaultRequestTimeout + handler.DefaultTimeoutGrace + 5*time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			a.logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	a.logger.Info("paprika-mcp-go starting", "port", cfg.Port, "writes_enabled", cfg.WritesEnabled)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-drained
	return nil
}
