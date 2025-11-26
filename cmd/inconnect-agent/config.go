package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
)

// Config captures all runtime configuration for the agent.
type Config struct {
	DBPath        string
	MinPort       int
	MaxPort       int
	ConfigDir     string
	ConfigFile    string
	GeneratedFile string
	ListenAddr    string
	PublicIP      string
	AuthToken     string
	ContainerName string
	DockerImage   string
	DockerBinary  string
	Method        string
	APIPort       int
}

func defaultConfig() Config {
	return Config{
		DBPath:        "/var/lib/inconnect-agent/ports.db",
		MinPort:       50001,
		MaxPort:       50250,
		ConfigDir:     "/etc/xray",
		ConfigFile:    "config.json",
		GeneratedFile: "config.generated.json",
		ListenAddr:    "127.0.0.1:8080",
		PublicIP:      "",
		AuthToken:     "",
		ContainerName: "xray-ss2022",
		DockerImage:   "teddysun/xray:latest",
		DockerBinary:  "docker",
		Method:        "2022-blake3-aes-128-gcm",
		APIPort:       10085,
	}
}

func (c *Config) registerFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.DBPath, "db-path", c.DBPath, "Path to SQLite database file")
	fs.IntVar(&c.MinPort, "min-port", c.MinPort, "First managed port (inclusive)")
	fs.IntVar(&c.MaxPort, "max-port", c.MaxPort, "Last managed port (inclusive)")
	fs.StringVar(&c.ConfigDir, "config-dir", c.ConfigDir, "Directory that stores Xray configs")
	fs.StringVar(&c.ConfigFile, "config-file", c.ConfigFile, "Final Xray config filename")
	fs.StringVar(&c.GeneratedFile, "generated-file", c.GeneratedFile, "Temporary config filename before swap")
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "HTTP listen address")
	fs.StringVar(&c.PublicIP, "public-ip", c.PublicIP, "Public IP exposed in /adduser responses")
	fs.StringVar(&c.AuthToken, "auth-token", c.AuthToken, "Optional X-Auth-Token required for requests")
	fs.StringVar(&c.ContainerName, "container-name", c.ContainerName, "Docker container name for Xray")
	fs.StringVar(&c.DockerImage, "docker-image", c.DockerImage, "Docker image to use for Xray runs")
	fs.StringVar(&c.DockerBinary, "docker-binary", c.DockerBinary, "Docker binary path")
	fs.StringVar(&c.Method, "method", c.Method, "Shadowsocks 2022 cipher method")
	fs.IntVar(&c.APIPort, "api-port", c.APIPort, "Xray API inbound port")
}

func (c Config) validate() error {
	if c.MinPort <= 0 || c.MaxPort <= 0 {
		return errors.New("ports must be positive")
	}
	if c.MinPort > c.MaxPort {
		return fmt.Errorf("min-port (%d) is greater than max-port (%d)", c.MinPort, c.MaxPort)
	}
	if c.ListenAddr == "" {
		return errors.New("listen address is required")
	}
	if c.ConfigDir == "" {
		return errors.New("config directory is required")
	}
	if filepath.Ext(c.ConfigFile) == "" {
		return errors.New("config file name must include extension")
	}
	if filepath.Ext(c.GeneratedFile) == "" {
		return errors.New("generated file name must include extension")
	}
	return nil
}

func (c Config) portCount() int {
	return c.MaxPort - c.MinPort + 1
}

func (c Config) generatedConfigPath() string {
	return filepath.Join(c.ConfigDir, c.GeneratedFile)
}

func (c Config) activeConfigPath() string {
	return filepath.Join(c.ConfigDir, c.ConfigFile)
}
