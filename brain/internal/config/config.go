package config

import (
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type MQTTConfig struct {
	Broker   string `yaml:"broker"`
	ClientID string `yaml:"client_id"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type PlantConfig struct {
	DisplayName     string  `yaml:"display_name"`
	MAC             string  `yaml:"mac"`
	MinMoisture     float64 `yaml:"min_moisture"`
	MaxMoisture     float64 `yaml:"max_moisture"`
	MinTemp         float64 `yaml:"min_temp"`
	MaxTemp         float64 `yaml:"max_temp"`
	CooldownSeconds int     `yaml:"cooldown_seconds"`  // converted to time.Duration on use
	MaxWaterSeconds int     `yaml:"max_water_seconds"` // hard pump-on cap; safety backstop
}

// CooldownPeriod returns CooldownSeconds as a time.Duration, defaulting to
// 20 minutes when the field is zero (i.e. omitted from config.yaml).
func (p PlantConfig) CooldownPeriod() time.Duration {
	if p.CooldownSeconds <= 0 {
		return 20 * time.Minute
	}
	return time.Duration(p.CooldownSeconds) * time.Second
}

// MaxWaterPeriod returns MaxWaterSeconds as a time.Duration, defaulting to
// 60 seconds when the field is zero. This is the brain-side safety cap on a
// single watering run; it should be shorter than the edge node's own hardware
// timer so the brain shuts off cleanly first in the normal online case.
func (p PlantConfig) MaxWaterPeriod() time.Duration {
	if p.MaxWaterSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(p.MaxWaterSeconds) * time.Second
}

// NotificationConfig holds SMTP and alerting behaviour parameters.
type NotificationConfig struct {
	Enabled         bool   `yaml:"enabled"`
	SMTPHost        string `yaml:"smtp_host"`
	SMTPPort        int    `yaml:"smtp_port"`
	Username        string `yaml:"username"`
	Password        string `yaml:"password"`
	From            string `yaml:"from"`      // display From address; defaults to Username
	Recipient       string `yaml:"recipient"` // destination address
	ReNotifyHours   int    `yaml:"re_notify_hours"`   // default 4
	WatchdogMinutes int    `yaml:"watchdog_minutes"`  // default 20
}

// ReNotifyInterval returns the re-notification interval, defaulting to 4 hours.
func (n NotificationConfig) ReNotifyInterval() time.Duration {
	if n.ReNotifyHours <= 0 {
		return 4 * time.Hour
	}
	return time.Duration(n.ReNotifyHours) * time.Hour
}

// WatchdogTimeout returns the node-offline threshold, defaulting to 20 minutes.
func (n NotificationConfig) WatchdogTimeout() time.Duration {
	if n.WatchdogMinutes <= 0 {
		return 20 * time.Minute
	}
	return time.Duration(n.WatchdogMinutes) * time.Minute
}

type Config struct {
	MQTT          MQTTConfig          `yaml:"mqtt"`
	Notifications NotificationConfig  `yaml:"notifications"`
	Plants        map[string]PlantConfig `yaml:"plants"`

	// mu guards concurrent access to Plants. Telemetry/alert reads run on
	// different goroutines than admin provisioning writes.
	mu sync.RWMutex `yaml:"-"`
}

// GetPlant returns the config for a plant under a read lock.
func (c *Config) GetPlant(id string) (PlantConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.Plants[id]
	return p, ok
}

// SetPlant stores the config for a plant under a write lock.
func (c *Config) SetPlant(id string, p PlantConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Plants[id] = p
}

// PlantIDs returns the IDs of all configured plants under a read lock.
func (c *Config) PlantIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.Plants))
	for id := range c.Plants {
		ids = append(ids, id)
	}
	return ids
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return &cfg, nil
}

// Save writes the config back to disk (used when admin provisions a new plant).
func Save(path string, cfg *Config) error {
	cfg.mu.RLock()
	data, err := yaml.Marshal(cfg)
	cfg.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
