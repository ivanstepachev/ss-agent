package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Agent ties together storage, config generation, docker orchestration, and HTTP handling.
type Agent struct {
	cfg      Config
	store    *SlotStore
	docker   *DockerManager
	shards   []ShardDefinition
	shardMap map[int]ShardDefinition
	reloadM  sync.Mutex
}

func NewAgent(cfg Config, shards []ShardDefinition, store *SlotStore, docker *DockerManager) *Agent {
	shardMap := make(map[int]ShardDefinition, len(shards))
	for _, sh := range shards {
		shardMap[sh.ID] = sh
	}
	return &Agent{
		cfg:      cfg,
		store:    store,
		docker:   docker,
		shards:   shards,
		shardMap: shardMap,
	}
}

func (a *Agent) shardList(target []int) ([]ShardDefinition, error) {
	if len(target) == 0 {
		return a.shards, nil
	}
	defs := make([]ShardDefinition, 0, len(target))
	for _, id := range target {
		sh, ok := a.shardMap[id]
		if !ok {
			return nil, fmt.Errorf("unknown shard_id %d", id)
		}
		defs = append(defs, sh)
	}
	return defs, nil
}

func (a *Agent) Reload(ctx context.Context, rotateReserved bool, target []int) (map[int]int, error) {
	a.reloadM.Lock()
	defer a.reloadM.Unlock()

	shards, err := a.shardList(target)
	if err != nil {
		return nil, err
	}

	results := make(map[int]int, len(shards))
	for _, shard := range shards {
		count, err := a.reloadShard(ctx, shard, rotateReserved)
		if err != nil {
			return results, err
		}
		results[shard.ID] = count
	}
	return results, nil
}

func (a *Agent) Restart(ctx context.Context, target []int) error {
	a.reloadM.Lock()
	defer a.reloadM.Unlock()

	shards, err := a.shardList(target)
	if err != nil {
		return err
	}
	for _, shard := range shards {
		if err := a.docker.FullRestartShard(ctx, a.cfg, shard); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) reloadShard(ctx context.Context, shard ShardDefinition, rotate bool) (int, error) {
	var processed int
	if rotate {
		count, err := a.store.RotateReserved(ctx, shard.ID)
		if err != nil {
			return 0, err
		}
		processed = count
	}

	slots, err := a.store.SlotsByShard(ctx, shard.ID, shard.SlotCount)
	if err != nil {
		return processed, err
	}

	payload, err := buildXrayConfig(slots, shard, a.cfg, a.store.ServerPassword(shard.ID))
	if err != nil {
		return processed, fmt.Errorf("build config shard %d: %w", shard.ID, err)
	}

	genPath := a.cfg.shardGeneratedPath(shard.ID)
	if err := os.WriteFile(genPath, payload, 0o640); err != nil {
		return processed, fmt.Errorf("write config shard %d: %w", shard.ID, err)
	}

	if err := a.docker.TestShard(ctx, a.cfg, shard); err != nil {
		_ = os.Remove(genPath)
		return processed, err
	}

	if err := os.Rename(genPath, a.cfg.shardConfigPath(shard.ID)); err != nil {
		_ = os.Remove(genPath)
		return processed, fmt.Errorf("activate config shard %d: %w", shard.ID, err)
	}

	if err := a.docker.ApplyShard(ctx, a.cfg, shard); err != nil {
		return processed, err
	}

	log.Printf("shard %d config updated", shard.ID)
	return processed, nil
}

func (a *Agent) StartAutoReload(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := a.Reload(context.Background(), true, nil); err != nil {
					log.Printf("auto reload failed: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

type xrayConfig struct {
	API       apiConfig      `json:"api"`
	Routing   routingConfig  `json:"routing"`
	Policy    policyConfig   `json:"policy"`
	Inbounds  []inbound      `json:"inbounds"`
	Outbounds []outbound     `json:"outbounds"`
	Stats     map[string]any `json:"stats"`
}

type apiConfig struct {
	Tag      string   `json:"tag"`
	Services []string `json:"services"`
}

type routingConfig struct {
	Rules []routingRule `json:"rules"`
}

type routingRule struct {
	InboundTag  []string `json:"inboundTag"`
	OutboundTag string   `json:"outboundTag"`
	Type        string   `json:"type"`
}

type policyConfig struct {
	Levels map[string]policyLevel `json:"levels"`
	System policySystem           `json:"system"`
}

type policyLevel struct {
	StatsUserUplink   bool `json:"statsUserUplink"`
	StatsUserDownlink bool `json:"statsUserDownlink"`
}

type policySystem struct {
	StatsInboundUplink    bool `json:"statsInboundUplink"`
	StatsInboundDownlink  bool `json:"statsInboundDownlink"`
	StatsOutboundUplink   bool `json:"statsOutboundUplink"`
	StatsOutboundDownlink bool `json:"statsOutboundDownlink"`
}

type inbound struct {
	Listen   string         `json:"listen,omitempty"`
	Port     int            `json:"port"`
	Protocol string         `json:"protocol"`
	Settings map[string]any `json:"settings"`
	Tag      string         `json:"tag,omitempty"`
}

type outbound struct {
	Protocol string `json:"protocol"`
	Tag      string `json:"tag,omitempty"`
}

type ssClient struct {
	Password string `json:"password"`
	Email    string `json:"email,omitempty"`
}

func buildXrayConfig(slots []Slot, shard ShardDefinition, cfg Config, serverPassword string) ([]byte, error) {
	clients := make([]ssClient, 0, len(slots))
	for _, slot := range slots {
		email := fmt.Sprintf("slot-%d", slot.ID)
		if slot.UserID.Valid && slot.UserID.String != "" {
			email = slot.UserID.String
		}
		clients = append(clients, ssClient{
			Password: slot.Password,
			Email:    email,
		})
	}

	inbounds := []inbound{
		{
			Listen:   "0.0.0.0",
			Port:     shard.Port,
			Protocol: "shadowsocks",
			Settings: map[string]any{
				"method":   cfg.Method,
				"password": serverPassword,
				"network":  "tcp,udp",
				"clients":  clients,
			},
		},
	}

	if shard.APIPort > 0 {
		inbounds = append(inbounds, inbound{
			Listen:   "0.0.0.0",
			Port:     shard.APIPort,
			Protocol: "dokodemo-door",
			Settings: map[string]any{
				"address": "0.0.0.0",
			},
			Tag: "api",
		})
	}

	cfgPayload := xrayConfig{
		API: apiConfig{
			Tag:      "api",
			Services: []string{"HandlerService", "LoggerService", "StatsService"},
		},
		Routing: routingConfig{
			Rules: []routingRule{
				{
					InboundTag:  []string{"api"},
					OutboundTag: "api",
					Type:        "field",
				},
			},
		},
		Policy: policyConfig{
			Levels: map[string]policyLevel{
				"1": {
					StatsUserUplink:   true,
					StatsUserDownlink: true,
				},
			},
			System: policySystem{
				StatsInboundUplink:    true,
				StatsInboundDownlink:  true,
				StatsOutboundUplink:   true,
				StatsOutboundDownlink: true,
			},
		},
		Inbounds: inbounds,
		Outbounds: []outbound{
			{Protocol: "freedom"},
			{Protocol: "dns", Tag: "api"},
		},
		Stats: map[string]any{},
	}

	bytes, err := json.MarshalIndent(cfgPayload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return bytes, nil
}

// DockerManager abstracts docker CLI interactions needed by the agent.
type DockerManager struct {
	Binary string
	Image  string
}

func (d *DockerManager) TestShard(ctx context.Context, cfg Config, shard ShardDefinition) error {
	args := []string{
		"run",
		"--rm",
		"-v",
		fmt.Sprintf("%s:/etc/xray", cfg.ConfigDir),
		d.Image,
		"xray",
		"-test",
		"-config",
		filepath.ToSlash(filepath.Join("/etc/xray", filepath.Base(cfg.shardGeneratedPath(shard.ID)))),
	}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("xray config validation failed (shard %d): %w", shard.ID, err)
	}
	return nil
}

func (d *DockerManager) ApplyShard(ctx context.Context, cfg Config, shard ShardDefinition) error {
	exists, err := d.containerExists(ctx, shard.ContainerName)
	if err != nil {
		return err
	}
	if !exists {
		log.Printf("docker container %s not found, creating", shard.ContainerName)
		return d.createContainer(ctx, cfg, shard)
	}
	if err := d.sendSignal(ctx, shard.ContainerName, "SIGUSR1"); err != nil {
		log.Printf("failed to signal container %s, falling back to restart: %v", shard.ContainerName, err)
		if err := d.restartContainer(ctx, shard.ContainerName); err != nil {
			return err
		}
	}
	return nil
}

func (d *DockerManager) FullRestartShard(ctx context.Context, cfg Config, shard ShardDefinition) error {
	exists, err := d.containerExists(ctx, shard.ContainerName)
	if err != nil {
		return err
	}
	if !exists {
		log.Printf("docker container %s not found, creating", shard.ContainerName)
		return d.createContainer(ctx, cfg, shard)
	}
	return d.restartContainer(ctx, shard.ContainerName)
}

func (d *DockerManager) containerExists(ctx context.Context, name string) (bool, error) {
	args := []string{"inspect", name}
	cmdErr := runCommand(ctx, d.Binary, args)
	if cmdErr != nil {
		var exitErr *commandError
		if errors.As(cmdErr, &exitErr) && exitErr.ExitCode == 1 {
			return false, nil
		}
		return false, fmt.Errorf("inspect container %s: %w", name, cmdErr)
	}
	return true, nil
}

func (d *DockerManager) restartContainer(ctx context.Context, name string) error {
	args := []string{"restart", name}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("restart container %s: %w", name, err)
	}
	return nil
}

func (d *DockerManager) sendSignal(ctx context.Context, name, signal string) error {
	args := []string{"kill", "--signal", signal, name}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("send signal %s to %s: %w", signal, name, err)
	}
	return nil
}

func (d *DockerManager) createContainer(ctx context.Context, cfg Config, shard ShardDefinition) error {
	args := []string{
		"run",
		"-d",
		"--name", shard.ContainerName,
		"--restart=always",
		"-v", fmt.Sprintf("%s:/etc/xray", cfg.ConfigDir),
		"-p", fmt.Sprintf("%d:%d/tcp", shard.Port, shard.Port),
		"-p", fmt.Sprintf("%d:%d/udp", shard.Port, shard.Port),
	}
	if shard.APIPort > 0 {
		args = append(args, "-p", fmt.Sprintf("%d:%d/tcp", shard.APIPort, shard.APIPort))
	}
	args = append(args,
		d.Image,
		"xray",
		"-config",
		filepath.ToSlash(filepath.Join("/etc/xray", filepath.Base(cfg.shardConfigPath(shard.ID)))),
	)
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("create container %s: %w", shard.ContainerName, err)
	}
	return nil
}

type commandError struct {
	Cmd      string
	Args     []string
	Output   string
	ExitCode int
	err      error
}

func (e *commandError) Error() string {
	return fmt.Sprintf("%s %v failed (exit %d): %v: %s", e.Cmd, e.Args, e.ExitCode, e.err, e.Output)
}

func runCommand(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	return &commandError{
		Cmd:      bin,
		Args:     append([]string{}, args...),
		Output:   string(output),
		ExitCode: exitCode,
		err:      err,
	}
}
