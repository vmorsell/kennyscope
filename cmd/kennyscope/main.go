// kennyscope watches Kenny's container logs from outside and renders
// a per-life timeline. Runs as a sibling Coolify service; uses
// tecnativa/docker-socket-proxy to reach Docker with least privilege.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vmorsell/kennyscope/internal/docker"
	"github.com/vmorsell/kennyscope/internal/store"
	"github.com/vmorsell/kennyscope/internal/tailer"
	"github.com/vmorsell/kennyscope/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	stateDir := envDefault("STATE_DIR", "/state")
	httpAddr := envDefault("HTTP_ADDR", ":8080")
	dockerHost := envDefault("DOCKER_HOST", "http://docker-socket-proxy:2375")
	match := envDefault("KENNY_CONTAINER_MATCH", "kenny")
	basicUser := os.Getenv("OBSERVER_USER")
	basicPass := os.Getenv("OBSERVER_PASSWORD")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		logger.Error("ensure state dir", slog.String("err", err.Error()))
		os.Exit(1)
	}

	s, err := store.Open(ctx, filepath.Join(stateDir, "observer.db"))
	if err != nil {
		logger.Error("open store", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer s.Close()

	dc := docker.New(dockerHost)
	tl := tailer.New(tailer.Config{
		Client: dc,
		Store:  s,
		Match:  match,
		Logger: logger.With(slog.String("component", "tailer")),
	})

	srv, err := web.New(web.Config{
		Addr:          httpAddr,
		Store:         s,
		BasicAuthUser: basicUser,
		BasicAuthPass: basicPass,
	})
	if err != nil {
		logger.Error("build web server", slog.String("err", err.Error()))
		os.Exit(1)
	}
	srv.Start()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("kennyscope.boot",
		slog.String("http_addr", httpAddr),
		slog.String("docker_host", dockerHost),
		slog.String("container_match", match),
		slog.Bool("basic_auth", basicUser != "" && basicPass != ""),
	)

	tl.Run(ctx)

	logger.Info("kennyscope.shutdown", slog.String("ctx_err", ctxErrString(ctx)))
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func ctxErrString(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return err.Error()
	}
	return ""
}
