package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config captures all runtime configuration for the agent.
type Config struct {
	DBPath                  string   `yaml:"dbPath"`
	MinPort                 int      `yaml:"minPort"`
	MaxPort                 int      `yaml:"maxPort"`
	ConfigDir               string   `yaml:"configDir"`
	ConfigFile              string   `yaml:"configFile"`
	GeneratedFile           string   `yaml:"generatedFile"`
	ListenAddr              string   `yaml:"listen"`
	PublicIP                string   `yaml:"publicIP"`
	AuthToken               string   `yaml:"authToken"`
	ContainerName           string   `yaml:"containerName"`
	DockerImage             string   `yaml:"dockerImage"`
	DockerBinary            string   `yaml:"dockerBinary"`
	Method                  string   `yaml:"method"`
	APIPort                 int      `yaml:"apiPort"`
	ShardCount              int      `yaml:"shardCount"`
	ShardSize               int      `yaml:"shardSize"`
	ShardPortStep           int      `yaml:"shardPortStep"`
	ShardRaw                string   `yaml:"shards"`
	ShardPrefix             string   `yaml:"shardPrefix"`
	RestartSeconds          int      `yaml:"restartInterval"`
	RestartReservedPerShard int      `yaml:"restartWhenReserved"`
	RestartAtUTC            []string `yaml:"restartAt"`
	AllocStrategy           string   `yaml:"allocationStrategy"`
	ResetOnly               bool     `yaml:"reset"`
}

func defaultConfig() Config {
	return Config{
		DBPath:                  "/var/lib/inconnect-agent/ports.db",
		MinPort:                 50001,
		MaxPort:                 50250,
		ConfigDir:               "/etc/xray",
		ConfigFile:              "config.json",
		GeneratedFile:           "config.generated.json",
		ListenAddr:              "127.0.0.1:8080",
		PublicIP:                "",
		AuthToken:               "",
		ContainerName:           "xray-ss2022",
		DockerImage:             "teddysun/xray:latest",
		DockerBinary:            "docker",
		Method:                  "2022-blake3-aes-128-gcm",
		APIPort:                 10085,
		ShardCount:              1,
		ShardSize:               0,
		ShardPortStep:           1,
		ShardPrefix:             "xray-ss2022",
		RestartSeconds:          0,
		RestartReservedPerShard: 0,
		RestartAtUTC:            nil,
		AllocStrategy:           "roundrobin",
		ResetOnly:               false,
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
	fs.StringVar(&c.ContainerName, "container-name", c.ContainerName, "Docker container name (legacy single-shard)")
	fs.StringVar(&c.DockerImage, "docker-image", c.DockerImage, "Docker image to use for Xray runs")
	fs.StringVar(&c.DockerBinary, "docker-binary", c.DockerBinary, "Docker binary path")
	fs.StringVar(&c.Method, "method", c.Method, "Shadowsocks 2022 cipher method")
	fs.IntVar(&c.APIPort, "api-port", c.APIPort, "Xray API inbound port")
	fs.IntVar(&c.ShardCount, "shard-count", c.ShardCount, "Number of Xray shards (containers)")
	fs.IntVar(&c.ShardSize, "shard-size", c.ShardSize, "Slots per shard (defaults to total slot count)")
	fs.IntVar(&c.ShardPortStep, "shard-port-step", c.ShardPortStep, "Port increment between shards")
	fs.StringVar(&c.ShardRaw, "shards", c.ShardRaw, "Custom shard definitions port:slots,... (overrides shard-count)")
	fs.StringVar(&c.ShardPrefix, "shard-prefix", c.ShardPrefix, "Prefix for shard container names")
	fs.IntVar(&c.RestartSeconds, "restart-interval", c.RestartSeconds, "Automatic restart interval in seconds (0 disables)")
	fs.IntVar(&c.RestartReservedPerShard, "restart-when-reserved", c.RestartReservedPerShard, "Trigger restart for a shard once reserved slots reach this number (0 disables)")
	fs.Func("restart-at", "Comma-separated UTC times (HH:MM) for full restarts", func(v string) error {
		if strings.TrimSpace(v) == "" {
			c.RestartAtUTC = nil
			return nil
		}
		parts := strings.Split(v, ",")
		var times []string
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, err := time.Parse("15:04", part); err != nil {
				return fmt.Errorf("invalid restart-at time %q: %w", part, err)
			}
			times = append(times, part)
		}
		c.RestartAtUTC = times
		return nil
	})
	fs.StringVar(&c.AllocStrategy, "allocation-strategy", c.AllocStrategy, "Slot allocation strategy: sequential|roundrobin|leastfree")
	fs.BoolVar(&c.ResetOnly, "reset", c.ResetOnly, "Reset database and shards, then exit")
}

func (c Config) validate() error {
	validAlloc := map[string]bool{"sequential": true, "roundrobin": true, "leastfree": true}
	if !validAlloc[c.AllocStrategy] {
		return fmt.Errorf("invalid allocation-strategy %q", c.AllocStrategy)
	}
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
	for _, t := range c.RestartAtUTC {
		if _, err := time.Parse("15:04", t); err != nil {
			return fmt.Errorf("invalid restart-at time %q: %w", t, err)
		}
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

type ShardDefinition struct {
	ID            int
	Port          int
	SlotCount     int
	ContainerName string
	APIPort       int
}

func (c Config) shardConfigPath(shardID int) string {
	name := fmt.Sprintf("config-shard-%d.json", shardID)
	return filepath.Join(c.ConfigDir, name)
}

func (c Config) shardGeneratedPath(shardID int) string {
	name := fmt.Sprintf("config-shard-%d.generated.json", shardID)
	return filepath.Join(c.ConfigDir, name)
}

func (c Config) shardContainer(shardID int) string {
	return fmt.Sprintf("%s-%d", c.ShardPrefix, shardID)
}

func (c Config) shardAPIPortFor(id int) int {
	if c.APIPort == 0 {
		return 0
	}
	return c.APIPort + id - 1
}

func (c Config) defaultShardSize() int {
	if c.ShardSize > 0 {
		return c.ShardSize
	}
	return c.portCount()
}

func (c Config) defaultShardCount() int {
	if c.ShardCount > 0 {
		return c.ShardCount
	}
	return 1
}

func (c Config) shardsFromRaw() ([]ShardDefinition, error) {
	if strings.TrimSpace(c.ShardRaw) == "" {
		return nil, nil
	}
	parts := strings.Split(c.ShardRaw, ",")
	var defs []ShardDefinition
	for idx, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sub := strings.Split(part, ":")
		if len(sub) != 2 {
			return nil, fmt.Errorf("invalid shard format %q, expected port:slots", part)
		}
		port, err := strconv.Atoi(sub[0])
		if err != nil {
			return nil, fmt.Errorf("invalid shard port %q: %w", sub[0], err)
		}
		slots, err := strconv.Atoi(sub[1])
		if err != nil {
			return nil, fmt.Errorf("invalid shard slot count %q: %w", sub[1], err)
		}
		if slots <= 0 {
			return nil, fmt.Errorf("shard slots must be positive for %q", part)
		}
		defs = append(defs, ShardDefinition{
			ID:            idx + 1,
			Port:          port,
			SlotCount:     slots,
			ContainerName: c.shardContainer(idx + 1),
			APIPort:       c.shardAPIPortFor(idx + 1),
		})
	}
	if len(defs) == 0 {
		return nil, errors.New("no valid shard definitions provided")
	}
	return defs, nil
}

func (c Config) BuildShards() ([]ShardDefinition, error) {
	if defs, err := c.shardsFromRaw(); err != nil {
		return nil, err
	} else if defs != nil {
		return defs, nil
	}
	size := c.defaultShardSize()
	count := c.defaultShardCount()
	if size <= 0 || count <= 0 {
		return nil, errors.New("invalid shard size/count configuration")
	}
	var defs []ShardDefinition
	for i := 0; i < count; i++ {
		id := i + 1
		port := c.MinPort + i*c.ShardPortStep
		defs = append(defs, ShardDefinition{
			ID:            id,
			Port:          port,
			SlotCount:     size,
			ContainerName: c.shardContainer(id),
			APIPort:       c.shardAPIPortFor(id),
		})
	}
	return defs, nil
}
