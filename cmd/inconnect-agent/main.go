package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	cfg := defaultConfig()
	cfg.registerFlags(flag.CommandLine)
	flag.Parse()

	if err := cfg.validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	if err := ensureParentDir(cfg.DBPath); err != nil {
		log.Fatalf("ensure db dir: %v", err)
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0o755); err != nil {
		log.Fatalf("ensure config dir: %v", err)
	}

	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=1", cfg.DBPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	shards, err := cfg.BuildShards()
	if err != nil {
		log.Fatalf("invalid shard configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewSlotStore(db, cfg.AllocStrategy, shards)
	if err := store.Init(ctx, cfg, shards); err != nil {
		log.Fatalf("initialize store: %v", err)
	}

	dockerManager := &DockerManager{
		Binary: cfg.DockerBinary,
		Image:  cfg.DockerImage,
	}
	cleanupContainers(ctx, dockerManager, cfg, shards)
	agent := NewAgent(cfg, shards, store, dockerManager)

	if cfg.ResetOnly {
		if err := agent.HardReset(ctx); err != nil {
			log.Fatalf("hard reset failed: %v", err)
		}
		log.Printf("hard reset completed")
		return
	}

	if _, err := agent.Reload(ctx, false, nil); err != nil {
		log.Fatalf("initial config generation failed: %v", err)
	}

	if cfg.RestartSeconds > 0 {
		agent.StartAutoRestart(ctx, time.Duration(cfg.RestartSeconds)*time.Second)
	}
	if cfg.RestartReservedPerShard > 0 {
		agent.StartAutoRestartOnReserved(ctx, cfg.RestartReservedPerShard, time.Minute)
	}
	if len(cfg.RestartAtUTC) > 0 {
		agent.StartScheduledRestarts(ctx, cfg.RestartAtUTC)
	}

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      agent.Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("agent HTTP API listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	waitForShutdown(server, cancel)
}

func waitForShutdown(server *http.Server, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %s, shutting down", sig)
	cancel()

	ctx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

func ensureParentDir(filePath string) error {
	dir := filepath.Dir(filePath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func cleanupContainers(ctx context.Context, docker *DockerManager, cfg Config, shards []ShardDefinition) {
	// remove legacy single-container instance if present
	if cfg.ContainerName != "" {
		if err := docker.RemoveIfExists(ctx, cfg.ContainerName); err != nil {
			log.Printf("failed to remove legacy container %s: %v", cfg.ContainerName, err)
		}
	}
	for _, shard := range shards {
		if err := docker.RemoveIfExists(ctx, shard.ContainerName); err != nil {
			log.Printf("failed to remove shard container %s: %v", shard.ContainerName, err)
		}
	}
}
