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

	ctx := context.Background()
	store := NewSlotStore(db)
	if err := store.Init(ctx, cfg); err != nil {
		log.Fatalf("initialize store: %v", err)
	}

	dockerManager := &DockerManager{
		Binary:        cfg.DockerBinary,
		Image:         cfg.DockerImage,
		ContainerName: cfg.ContainerName,
	}
	agent := NewAgent(cfg, store, dockerManager)

	if _, err := agent.Reload(ctx, false); err != nil {
		log.Fatalf("initial config generation failed: %v", err)
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

	waitForShutdown(server)
}

func waitForShutdown(server *http.Server) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %s, shutting down", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
