package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"gopkg.in/yaml.v3"
)

// Config structures
type BrokerConfig struct {
	Address  string `yaml:"address"`
	ClientID string `yaml:"client_id"`
}

type PlantThresholds struct {
	MinMoisture float64 `yaml:"min_moisture"`
	MaxMoisture float64 `yaml:"max_moisture"`
	MinTemp     float64 `yaml:"min_temp"`
	MaxTemp     float64 `yaml:"max_temp"`
}

type Config struct {
	Broker BrokerConfig                `yaml:"broker"`
	Plants map[string]PlantThresholds `yaml:"plants"`
}

// Telemetry message from edge nodes
type TelemetryMessage struct {
	PlantID  string  `json:"plant_id"`
	Moisture float64 `json:"moisture"`
	Temp     float64 `json:"temp"`
}

// Setup message from unprovisioned devices
type SetupMessage struct {
	MAC    string `json:"mac"`
	Status string `json:"status"`
}

// Admin message from TUI
type AdminMessage struct {
	Action      string  `json:"action"`
	MAC         string  `json:"mac"`
	PlantID     string  `json:"plant_id"`
	MinMoisture float64 `json:"min_moisture,string"`
	MaxMoisture float64 `json:"max_moisture,string"`
	MinTemp     float64 `json:"min_temp,string"`
	MaxTemp     float64 `json:"max_temp,string"`
}

// Master state structures
type PlantState struct {
	Moisture      float64 `json:"moisture"`
	Temp          float64 `json:"temp"`
	MoistureAlert bool    `json:"moisture_alert"`
	TempAlert     bool    `json:"temp_alert"`
	AlertMsg      string  `json:"alert_msg,omitempty"`
}

type MasterState struct {
	Timestamp             int64                  `json:"timestamp"`
	Plants                map[string]PlantState  `json:"plants"`
	UnprovisionedDevices  []string               `json:"unprovisioned_devices"`
}

// Brain daemon
type Brain struct {
	config                Config
	configPath            string
	client                mqtt.Client
	telemetryCache        map[string]TelemetryMessage
	unprovisionedDevices  map[string]bool
	mu                    sync.RWMutex
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

func (b *Brain) handleTelemetry(_ mqtt.Client, msg mqtt.Message) {
	var telemetry TelemetryMessage
	if err := json.Unmarshal(msg.Payload(), &telemetry); err != nil {
		log.Printf("Failed to parse telemetry: %v", err)
		return
	}

	b.mu.Lock()
	b.telemetryCache[telemetry.PlantID] = telemetry
	b.mu.Unlock()

	log.Printf("Telemetry received: %s -> moisture=%.1f%%, temp=%.1f°C",
		telemetry.PlantID, telemetry.Moisture, telemetry.Temp)
}

func (b *Brain) handleSetup(_ mqtt.Client, msg mqtt.Message) {
	var setup SetupMessage
	if err := json.Unmarshal(msg.Payload(), &setup); err != nil {
		log.Printf("Failed to parse setup message: %v", err)
		return
	}

	if setup.Status == "awaiting_provision" {
		b.mu.Lock()
		b.unprovisionedDevices[setup.MAC] = true
		b.mu.Unlock()
		log.Printf("Unprovisioned device detected: %s", setup.MAC)
	}
}

func (b *Brain) handleAdmin(_ mqtt.Client, msg mqtt.Message) {
	var admin AdminMessage
	if err := json.Unmarshal(msg.Payload(), &admin); err != nil {
		log.Printf("Failed to parse admin message: %v", err)
		return
	}

	if admin.Action == "new_plant" {
		log.Printf("Provisioning request: %s -> %s", admin.MAC, admin.PlantID)

		// Update config in memory
		b.mu.Lock()
		b.config.Plants[admin.PlantID] = PlantThresholds{
			MinMoisture: admin.MinMoisture,
			MaxMoisture: admin.MaxMoisture,
			MinTemp:     admin.MinTemp,
			MaxTemp:     admin.MaxTemp,
		}

		// Remove from unprovisioned list
		delete(b.unprovisionedDevices, admin.MAC)
		b.mu.Unlock()

		// Save config to disk
		if err := b.saveConfig(); err != nil {
			log.Printf("Failed to save config: %v", err)
			return
		}

		// Send assignment to ESP32
		assignPayload := map[string]string{
			"assign_id": admin.PlantID,
		}
		assignJSON, _ := json.Marshal(assignPayload)

		topic := fmt.Sprintf("garden/setup/%s", admin.MAC)
		token := b.client.Publish(topic, 0, false, assignJSON)
		token.Wait()

		log.Printf("Assignment sent to %s: plant_id=%s", admin.MAC, admin.PlantID)
	}
}

func (b *Brain) saveConfig() error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	data, err := yaml.Marshal(&b.config)
	if err != nil {
		return err
	}

	if err := os.WriteFile(b.configPath, data, 0644); err != nil {
		return err
	}

	log.Printf("Config saved to %s", b.configPath)
	return nil
}

func (b *Brain) generateMasterState() MasterState {
	b.mu.RLock()
	defer b.mu.RUnlock()

	state := MasterState{
		Timestamp:            time.Now().Unix(),
		Plants:               make(map[string]PlantState),
		UnprovisionedDevices: make([]string, 0),
	}

	// Build plant states with alert logic
	for plantID, telemetry := range b.telemetryCache {
		thresholds, exists := b.config.Plants[plantID]
		if !exists {
			continue
		}

		plantState := PlantState{
			Moisture: telemetry.Moisture,
			Temp:     telemetry.Temp,
		}

		// Check moisture alerts
		if telemetry.Moisture < thresholds.MinMoisture {
			plantState.MoistureAlert = true
			plantState.AlertMsg = fmt.Sprintf("Moisture %.1f%% below minimum %.1f%%",
				telemetry.Moisture, thresholds.MinMoisture)
		} else if telemetry.Moisture > thresholds.MaxMoisture {
			plantState.MoistureAlert = true
			plantState.AlertMsg = fmt.Sprintf("Moisture %.1f%% exceeds %.1f%% max threshold",
				telemetry.Moisture, thresholds.MaxMoisture)
		}

		// Check temperature alerts
		if telemetry.Temp < thresholds.MinTemp {
			plantState.TempAlert = true
			if plantState.AlertMsg != "" {
				plantState.AlertMsg += "; "
			}
			plantState.AlertMsg += fmt.Sprintf("Temperature %.1f°C below minimum %.1f°C",
				telemetry.Temp, thresholds.MinTemp)
		} else if telemetry.Temp > thresholds.MaxTemp {
			plantState.TempAlert = true
			if plantState.AlertMsg != "" {
				plantState.AlertMsg += "; "
			}
			plantState.AlertMsg += fmt.Sprintf("Temperature %.1f°C exceeds %.1f°C max threshold",
				telemetry.Temp, thresholds.MaxTemp)
		}

		state.Plants[plantID] = plantState
	}

	// Add unprovisioned devices
	for mac := range b.unprovisionedDevices {
		state.UnprovisionedDevices = append(state.UnprovisionedDevices, mac)
	}

	return state
}

func (b *Brain) publishState() {
	state := b.generateMasterState()

	payload, err := json.Marshal(state)
	if err != nil {
		log.Printf("Failed to marshal state: %v", err)
		return
	}

	token := b.client.Publish("garden/state", 0, false, payload)
	token.Wait()

	log.Printf("Published master state: %d plants, %d unprovisioned devices",
		len(state.Plants), len(state.UnprovisionedDevices))
}

func (b *Brain) stateTicker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		b.publishState()
	}
}

func main() {
	// Load config
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("Loaded config: %d plants configured", len(config.Plants))

	// Create brain instance
	brain := &Brain{
		config:               *config,
		configPath:           "config.yaml",
		telemetryCache:       make(map[string]TelemetryMessage),
		unprovisionedDevices: make(map[string]bool),
	}

	// Setup MQTT client
	opts := mqtt.NewClientOptions()
	opts.AddBroker(config.Broker.Address)
	opts.SetClientID(config.Broker.ClientID)
	opts.SetAutoReconnect(true)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Println("Connected to MQTT broker")

		// Subscribe to telemetry
		if token := c.Subscribe("garden/telemetry", 0, brain.handleTelemetry); token.Wait() && token.Error() != nil {
			log.Fatalf("Failed to subscribe to telemetry: %v", token.Error())
		}
		log.Println("Subscribed to garden/telemetry")

		// Subscribe to setup
		if token := c.Subscribe("garden/setup", 0, brain.handleSetup); token.Wait() && token.Error() != nil {
			log.Fatalf("Failed to subscribe to setup: %v", token.Error())
		}
		log.Println("Subscribed to garden/setup")

		// Subscribe to admin
		if token := c.Subscribe("garden/admin", 0, brain.handleAdmin); token.Wait() && token.Error() != nil {
			log.Fatalf("Failed to subscribe to admin: %v", token.Error())
		}
		log.Println("Subscribed to garden/admin")
	})

	brain.client = mqtt.NewClient(opts)
	if token := brain.client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to connect to broker: %v", token.Error())
	}

	log.Println("Garden Brain daemon started")

	// Start state ticker
	go brain.stateTicker()

	// Keep running
	select {}
}
