package state

import (
	"sync"
	"time"

	"github.com/iot-herb-garden/brain/internal/domain"
)

// PlantState is the enriched, per-plant snapshot included in the master JSON.
// It joins raw telemetry with the controller's latest decision.
type PlantState struct {
	DisplayName   string  `json:"display_name,omitempty"`
	Moisture      float64 `json:"moisture"`
	Temp          float64 `json:"temp"`
	Watering      bool    `json:"watering"`
	InCooldown    bool    `json:"in_cooldown"`
	MoistureAlert bool    `json:"moisture_alert"`
	TempAlert     bool    `json:"temp_alert"`
	AlertMsg      string  `json:"alert_msg,omitempty"`
	LastSeen      int64   `json:"last_seen"` // Unix timestamp of last telemetry
}

// StatePayload is the master JSON published to garden/state every tick.
type StatePayload struct {
	Timestamp            int64                 `json:"timestamp"`
	Plants               map[string]PlantState `json:"plants"`
	UnprovisionedDevices []string              `json:"unprovisioned_devices"`
}

// Store is the thread-safe in-memory repository for the Brain's live state.
type Store struct {
	mu                   sync.RWMutex
	telemetry            map[string]domain.Telemetry // latest reading per plant
	decisions            map[string]domain.Decision  // latest decision per plant
	lastSeen             map[string]time.Time         // wall time of last telemetry receipt
	displayNames         map[string]string            // human-readable name per plant
	unprovisionedDevices map[string]struct{}           // keyed by MAC address
}

func NewStore() *Store {
	return &Store{
		telemetry:            make(map[string]domain.Telemetry),
		decisions:            make(map[string]domain.Decision),
		lastSeen:             make(map[string]time.Time),
		displayNames:         make(map[string]string),
		unprovisionedDevices: make(map[string]struct{}),
	}
}

// SetDisplayName records the human-readable name for a plant. Called by the
// MQTT handler whenever telemetry arrives for a known plant so that the name
// from config.yaml flows into the published garden/state payload.
func (s *Store) SetDisplayName(plantID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displayNames[plantID] = name
}

// UpdateTelemetry stores the latest sensor reading and records the wall-clock
// arrival time for the watchdog heartbeat check.
func (s *Store) UpdateTelemetry(t domain.Telemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.telemetry[t.PlantID] = t
	s.lastSeen[t.PlantID] = time.Now()
}

func (s *Store) UpdateDecision(d domain.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions[d.PlantID] = d
}

func (s *Store) AddUnprovisioned(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unprovisionedDevices[mac] = struct{}{}
}

func (s *Store) RemoveUnprovisioned(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unprovisionedDevices, mac)
}

// AllDecisions returns a snapshot copy of all current plant decisions.
// The AlertManager calls this on every tick to evaluate alert conditions.
func (s *Store) AllDecisions() map[string]domain.Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]domain.Decision, len(s.decisions))
	for k, v := range s.decisions {
		out[k] = v
	}
	return out
}

// AllLastSeen returns a snapshot copy of the last-telemetry timestamps for
// all known plants. The AlertManager uses these for the watchdog check.
func (s *Store) AllLastSeen() map[string]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]time.Time, len(s.lastSeen))
	for k, v := range s.lastSeen {
		out[k] = v
	}
	return out
}

// BuildStatePayload assembles the master state snapshot for publication.
// It joins stored telemetry with stored decisions; plants with no decision yet
// (e.g., first reading not yet evaluated) are included with zero decision fields.
func (s *Store) BuildStatePayload() StatePayload {
	s.mu.RLock()
	defer s.mu.RUnlock()

	plants := make(map[string]PlantState, len(s.telemetry))
	for id, t := range s.telemetry {
		ps := PlantState{
			DisplayName: s.displayNames[id],
			Moisture:    t.Moisture,
			Temp:        t.Temp,
			LastSeen:    s.lastSeen[id].Unix(),
		}
		if d, ok := s.decisions[id]; ok {
			ps.Watering = d.Watering
			ps.InCooldown = d.InCooldown
			ps.MoistureAlert = d.MoistureAlert
			ps.TempAlert = d.TempAlert
			ps.AlertMsg = d.AlertMsg
		}
		plants[id] = ps
	}

	list := make([]string, 0, len(s.unprovisionedDevices))
	for mac := range s.unprovisionedDevices {
		list = append(list, mac)
	}

	return StatePayload{
		Timestamp:            time.Now().Unix(),
		Plants:               plants,
		UnprovisionedDevices: list,
	}
}
