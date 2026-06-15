package mqtt

import (
	"encoding/json"
	"fmt"
	"log"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/iot-herb-garden/brain/internal/config"
	"github.com/iot-herb-garden/brain/internal/controller"
	"github.com/iot-herb-garden/brain/internal/domain"
	"github.com/iot-herb-garden/brain/internal/state"
)

// ── Wire types (MQTT payload shapes) ─────────────────────────────────────────

type telemetryPayload struct {
	PlantID  string  `json:"plant_id"`
	Moisture float64 `json:"moisture"`
	Temp     float64 `json:"temp"`
}

type setupPayload struct {
	MAC    string `json:"mac"`
	Status string `json:"status,omitempty"`
}

type adminPayload struct {
	Action          string  `json:"action"`
	MAC             string  `json:"mac"`
	PlantID         string  `json:"plant_id"`
	DisplayName     string  `json:"display_name"`
	MinMoisture     float64 `json:"min_moisture"`
	MaxMoisture     float64 `json:"max_moisture"`
	MinTemp         float64 `json:"min_temp"`
	MaxTemp         float64 `json:"max_temp"`
	CooldownSeconds int     `json:"cooldown_seconds"`
}

type assignPayload struct {
	AssignID string `json:"assign_id"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// HandleTelemetry decodes a sensor reading, stores it, runs it through the
// controller, and — only when the pump state changes — publishes a Command
// to garden/command/{plant_id}.
func HandleTelemetry(
	ctrl *controller.Controller,
	store *state.Store,
	cfg *config.Config,
	pub Publisher,
) pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, msg pahomqtt.Message) {
		var p telemetryPayload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("[telemetry] bad payload: %v", err)
			return
		}
		if p.PlantID == "" {
			log.Printf("[telemetry] missing plant_id, ignoring")
			return
		}

		t := domain.Telemetry{
			PlantID:  p.PlantID,
			Moisture: p.Moisture,
			Temp:     p.Temp,
		}
		store.UpdateTelemetry(t)

		plantCfg, ok := cfg.GetPlant(p.PlantID)
		if !ok {
			log.Printf("[telemetry] no config for plant_id %q, ignoring", p.PlantID)
			return
		}

		domainCfg := domain.PlantConfig{
			MinMoisture:    plantCfg.MinMoisture,
			MaxMoisture:    plantCfg.MaxMoisture,
			MinTemp:        plantCfg.MinTemp,
			MaxTemp:        plantCfg.MaxTemp,
			CooldownPeriod: plantCfg.CooldownPeriod(),
			MaxWaterPeriod: plantCfg.MaxWaterPeriod(),
		}

		store.SetDisplayName(p.PlantID, plantCfg.DisplayName)
		decision := ctrl.Evaluate(domainCfg, t)
		store.UpdateDecision(decision)

		log.Printf("[telemetry] %s → moisture=%.1f temp=%.1f watering=%v cooldown=%v",
			p.PlantID, p.Moisture, p.Temp, decision.Watering, decision.InCooldown)

		// Only publish a command to the edge node when the pump state flips.
		if decision.WateringChanged {
			action := "water_off"
			if decision.Watering {
				action = "water_on"
			}
			PublishCommand(pub, p.PlantID, action)
		}
	}
}

// PublishCommand sends a pump command to garden/command/{plantID} at QoS 1.
// Shared by the telemetry handler, the tick-driven safety enforcer, and the
// startup fail-safe so command formatting lives in one place.
func PublishCommand(pub Publisher, plantID, action string) {
	cmd, _ := json.Marshal(domain.Command{PlantID: plantID, Action: action})
	topic := fmt.Sprintf("garden/command/%s", plantID)
	pub.Publish(topic, 1, false, cmd)
	log.Printf("[command] published %q → %s", action, topic)
}

// HandleSetup tracks MAC addresses of unprovisioned edge nodes.
func HandleSetup(store *state.Store) pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, msg pahomqtt.Message) {
		var p setupPayload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("[setup] bad payload: %v", err)
			return
		}
		if p.Status == "awaiting_provision" && p.MAC != "" {
			store.AddUnprovisioned(p.MAC)
			log.Printf("[setup] unprovisioned device detected: %s", p.MAC)
		}
	}
}

// HandleAdmin processes provisioning commands from the TUI: writes to
// config.yaml, clears controller state for the plant, and sends assign_id
// back to the waiting ESP32.
func HandleAdmin(
	pub Publisher,
	store *state.Store,
	ctrl *controller.Controller,
	cfg *config.Config,
	cfgPath string,
) pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, msg pahomqtt.Message) {
		var p adminPayload
		if err := json.Unmarshal(msg.Payload(), &p); err != nil {
			log.Printf("[admin] bad payload: %v", err)
			return
		}
		if p.Action != "new_plant" {
			log.Printf("[admin] unknown action %q, ignoring", p.Action)
			return
		}
		if p.MAC == "" || p.PlantID == "" {
			log.Printf("[admin] new_plant missing mac or plant_id, ignoring")
			return
		}

		cfg.SetPlant(p.PlantID, config.PlantConfig{
			DisplayName:     p.DisplayName,
			MAC:             p.MAC,
			MinMoisture:     p.MinMoisture,
			MaxMoisture:     p.MaxMoisture,
			MinTemp:         p.MinTemp,
			MaxTemp:         p.MaxTemp,
			CooldownSeconds: p.CooldownSeconds,
		})
		if err := config.Save(cfgPath, cfg); err != nil {
			log.Printf("[admin] failed to save config: %v", err)
			return
		}

		store.RemoveUnprovisioned(p.MAC)
		store.SetDisplayName(p.PlantID, p.DisplayName)
		ctrl.Reset(p.PlantID) // clear stale hysteresis if plant was reprovisioned

		topic := fmt.Sprintf("garden/setup/%s", p.MAC)
		payload, _ := json.Marshal(assignPayload{AssignID: p.PlantID})
		pub.Publish(topic, 1, false, payload)

		log.Printf("[admin] provisioned %s → %s", p.MAC, p.PlantID)
	}
}
