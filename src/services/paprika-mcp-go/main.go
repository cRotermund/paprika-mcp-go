package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/config"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/handler"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mcpserver"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/middleware"
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

	if *stdio {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if err := mcpserver.New().Run(ctx, &mcp.StdioTransport{}); err != nil {
			logger.Error("stdio server failed", "error", err)
			os.Exit(1)
		}

		return
	}

	cfg, err := config.Load(logger)
	if err != nil {
		logger.Error("could not load configuration", "error", err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(middleware.Logger(logger))

	r.Get("/health", handler.Health())

	mcpServer := mcpserver.New()
	r.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("paprika-mcp-go starting", "port", cfg.Port)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server failed", "error", err)
		os.Exit(1)
	}

	logger.Info("paprika-mcp-go stopped")
}
