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
)

// Agent ties together storage, config generation, docker orchestration, and HTTP handling.
type Agent struct {
	cfg     Config
	store   *SlotStore
	docker  *DockerManager
	reloadM sync.Mutex
}

func NewAgent(cfg Config, store *SlotStore, docker *DockerManager) *Agent {
	return &Agent{
		cfg:    cfg,
		store:  store,
		docker: docker,
	}
}

func (a *Agent) Reload(ctx context.Context, rotateReserved bool) (int, error) {
	a.reloadM.Lock()
	defer a.reloadM.Unlock()

	var processed int
	var err error
	if rotateReserved {
		processed, err = a.store.RotateReserved(ctx)
		if err != nil {
			return 0, err
		}
	}

	if err := a.generateAndSwapConfig(ctx); err != nil {
		return processed, err
	}

	if err := a.docker.ApplyConfig(ctx, a.cfg); err != nil {
		return processed, err
	}

	return processed, nil
}

func (a *Agent) generateAndSwapConfig(ctx context.Context) error {
	slots, err := a.store.AllSlots(ctx, a.cfg.MinPort, a.cfg.MaxPort)
	if err != nil {
		return err
	}

	payload, err := buildXrayConfig(slots, a.cfg, a.store.ServerPassword())
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}

	generatedPath := a.cfg.generatedConfigPath()
	if err := os.WriteFile(generatedPath, payload, 0o640); err != nil {
		return fmt.Errorf("write generated config: %w", err)
	}

	if err := a.docker.TestConfig(ctx, a.cfg); err != nil {
		_ = os.Remove(generatedPath)
		return err
	}

	if err := os.Rename(generatedPath, a.cfg.activeConfigPath()); err != nil {
		_ = os.Remove(generatedPath)
		return fmt.Errorf("activate config: %w", err)
	}

	log.Printf("config updated at %s", a.cfg.activeConfigPath())
	return nil
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

func buildXrayConfig(slots []Slot, cfg Config, serverPassword string) ([]byte, error) {
	clients := make([]ssClient, 0, len(slots))
	for _, slot := range slots {
		email := fmt.Sprintf("slot-%d", slot.Port)
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
			Port:     cfg.MinPort,
			Protocol: "shadowsocks",
			Settings: map[string]any{
				"method":   cfg.Method,
				"password": serverPassword,
				"network":  "tcp,udp",
				"clients":  clients,
			},
		},
		{
			Listen:   "0.0.0.0",
			Port:     cfg.APIPort,
			Protocol: "dokodemo-door",
			Settings: map[string]any{
				"address": "0.0.0.0",
			},
			Tag: "api",
		},
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
	Binary        string
	Image         string
	ContainerName string
}

func (d *DockerManager) TestConfig(ctx context.Context, cfg Config) error {
	args := []string{
		"run",
		"--rm",
		"-v",
		fmt.Sprintf("%s:/etc/xray", cfg.ConfigDir),
		d.Image,
		"xray",
		"-test",
		"-config",
		fmt.Sprintf("/etc/xray/%s", cfg.GeneratedFile),
	}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("xray config validation failed: %w", err)
	}
	return nil
}

func (d *DockerManager) ApplyConfig(ctx context.Context, cfg Config) error {
	exists, err := d.containerExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		log.Printf("docker container %s not found, creating", d.ContainerName)
		return d.createContainer(ctx, cfg)
	}
	if err := d.sendSignal(ctx, "SIGUSR1"); err != nil {
		log.Printf("failed to signal container %s, falling back to restart: %v", d.ContainerName, err)
		if err := d.restartContainer(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (d *DockerManager) containerExists(ctx context.Context) (bool, error) {
	args := []string{"inspect", d.ContainerName}
	cmdErr := runCommand(ctx, d.Binary, args)
	if cmdErr != nil {
		var exitErr *commandError
		if errors.As(cmdErr, &exitErr) && exitErr.ExitCode == 1 {
			return false, nil
		}
		return false, fmt.Errorf("inspect container: %w", cmdErr)
	}
	return true, nil
}

func (d *DockerManager) restartContainer(ctx context.Context) error {
	args := []string{"restart", d.ContainerName}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("restart container: %w", err)
	}
	return nil
}

func (d *DockerManager) sendSignal(ctx context.Context, signal string) error {
	args := []string{"kill", "--signal", signal, d.ContainerName}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("send signal %s: %w", signal, err)
	}
	return nil
}

func (d *DockerManager) createContainer(ctx context.Context, cfg Config) error {
	args := []string{
		"run",
		"-d",
		"--name", d.ContainerName,
		"--restart=always",
		"-v", fmt.Sprintf("%s:/etc/xray", cfg.ConfigDir),
		"-p", fmt.Sprintf("%d:%d/tcp", cfg.MinPort, cfg.MinPort),
		"-p", fmt.Sprintf("%d:%d/udp", cfg.MinPort, cfg.MinPort),
		"-p", fmt.Sprintf("%d:%d/tcp", cfg.APIPort, cfg.APIPort),
		d.Image,
		"xray",
		"-config",
		filepath.ToSlash(filepath.Join("/etc/xray", cfg.ConfigFile)),
	}
	if err := runCommand(ctx, d.Binary, args); err != nil {
		return fmt.Errorf("create container: %w", err)
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
