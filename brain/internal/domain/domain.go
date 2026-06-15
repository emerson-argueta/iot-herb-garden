package domain

import "time"

// Telemetry is a single sensor reading arriving from an edge node.
type Telemetry struct {
	PlantID  string
	Moisture float64
	Temp     float64
}

// PlantConfig holds thresholds and operational parameters for one plant.
// CooldownPeriod is the minimum time the controller waits after turning the
// pump on before it will evaluate whether to turn it off again.
type PlantConfig struct {
	DisplayName    string
	MAC            string
	MinMoisture    float64
	MaxMoisture    float64
	MinTemp        float64
	MaxTemp        float64
	CooldownPeriod time.Duration
	// MaxWaterPeriod is the hard cap on how long the pump may run before the
	// controller forces it off, independent of telemetry. Safety backstop
	// against a stuck sensor, popped tube, or stalled telemetry.
	MaxWaterPeriod time.Duration
}

// Decision is the output of one Controller.Evaluate call.
// It carries both the desired actuator state and informational alert flags.
type Decision struct {
	PlantID         string
	Watering        bool   // desired pump state (true = on)
	WateringChanged bool   // true when the pump state flipped this cycle
	MoistureAlert   bool   // moisture is outside the acceptable band
	TempAlert       bool   // temperature is outside the acceptable band
	AlertMsg        string // human-readable summary, empty when no alerts
	InCooldown      bool   // controller is within the post-WaterOn quiet window
}

// Command is a control message the Brain publishes to an edge node.
// Published to garden/command/{plant_id}.
type Command struct {
	PlantID string `json:"plant_id"`
	Action  string `json:"action"` // "water_on" | "water_off"
}
