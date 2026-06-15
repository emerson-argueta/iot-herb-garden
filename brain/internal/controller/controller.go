// Package controller is the pure-logic engine for the herb garden Brain.
// It has zero MQTT dependencies and is designed to be fully unit-testable.
//
// Hysteresis model:
//   - WaterOn  fires when moisture < MinMoisture AND pump is currently OFF.
//   - WaterOff fires when moisture > MaxMoisture AND pump is currently ON
//     AND the cooldown window has elapsed.
//   - Any reading inside [MinMoisture, MaxMoisture] maintains the current
//     pump state without issuing a new command.
//
// Cooldown model:
//   - On WaterOn, a cooldown window [now, now+CooldownPeriod] is opened.
//   - While inside the window the controller skips moisture evaluation
//     entirely, preventing both spurious re-triggers and premature shutoff
//     while the soil is still absorbing water.
//   - Temperature alerts are evaluated on every call, independent of
//     cooldown state.
package controller

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iot-herb-garden/brain/internal/domain"
)

// Option is a functional option for [New].
type Option func(*Controller)

// WithClock injects a custom clock function. Use this in tests to control time
// without sleeping or relying on wall time.
//
//	var now time.Time
//	ctrl := controller.New(controller.WithClock(func() time.Time { return now }))
func WithClock(fn func() time.Time) Option {
	return func(c *Controller) { c.now = fn }
}

// Controller holds per-plant hysteresis and cooldown state.
// All exported methods are safe for concurrent use.
type Controller struct {
	mu            sync.Mutex
	watering      map[string]bool      // current pump state per plant
	cooledUntil   map[string]time.Time // cooldown expiry per plant
	waterDeadline map[string]time.Time // hard pump-off deadline per plant
	now           func() time.Time
}

// New creates a Controller with real wall time unless overridden by [WithClock].
func New(opts ...Option) *Controller {
	c := &Controller{
		watering:      make(map[string]bool),
		cooledUntil:   make(map[string]time.Time),
		waterDeadline: make(map[string]time.Time),
		now:           time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Evaluate applies the hysteresis and cooldown logic to a single telemetry
// reading and returns a [domain.Decision]. It mutates internal state when a
// pump transition occurs.
//
// Decision.WateringChanged == true signals the caller to publish a Command to
// the edge node. The caller must NOT publish on every call — only on changes.
func (c *Controller) Evaluate(cfg domain.PlantConfig, t domain.Telemetry) domain.Decision {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	id := t.PlantID
	wasWatering := c.watering[id]
	inCooldown := !now.After(c.cooledUntil[id])

	d := domain.Decision{
		PlantID:    id,
		Watering:   wasWatering,
		InCooldown: inCooldown,
		// Moisture alert is informational: outside bounds regardless of pump state.
		MoistureAlert: t.Moisture < cfg.MinMoisture || t.Moisture > cfg.MaxMoisture,
	}

	// --- Moisture / pump state machine ---
	switch {
	case !wasWatering && !inCooldown && t.Moisture < cfg.MinMoisture:
		// Soil is dry and pump is idle: start watering, open the cooldown window,
		// and arm the hard pump-off deadline as a safety backstop.
		c.watering[id] = true
		c.cooledUntil[id] = now.Add(cfg.CooldownPeriod)
		c.waterDeadline[id] = now.Add(cfg.MaxWaterPeriod)
		d.Watering = true
		d.WateringChanged = true
		d.InCooldown = true // cooldown begins this tick

	case wasWatering && !inCooldown && t.Moisture > cfg.MaxMoisture:
		// Cooldown has elapsed and soil is saturated: stop watering.
		c.watering[id] = false
		delete(c.waterDeadline, id)
		d.Watering = false
		d.WateringChanged = true
	}
	// All other cases (in-range, in-cooldown, or between thresholds while
	// watering) maintain the current pump state without issuing a command.

	// --- Temperature alerts (always evaluated) ---
	var alerts []string
	switch {
	case t.Temp < cfg.MinTemp:
		d.TempAlert = true
		alerts = append(alerts, fmt.Sprintf(
			"Temp %.1f°C below %.1f°C minimum", t.Temp, cfg.MinTemp))
	case t.Temp > cfg.MaxTemp:
		d.TempAlert = true
		alerts = append(alerts, fmt.Sprintf(
			"Temp %.1f°C exceeds %.1f°C maximum", t.Temp, cfg.MaxTemp))
	}
	if d.MoistureAlert {
		switch {
		case t.Moisture < cfg.MinMoisture:
			alerts = append(alerts, fmt.Sprintf(
				"Moisture %.1f%% below %.0f%% minimum", t.Moisture, cfg.MinMoisture))
		case t.Moisture > cfg.MaxMoisture:
			alerts = append(alerts, fmt.Sprintf(
				"Moisture %.1f%% exceeds %.0f%% maximum", t.Moisture, cfg.MaxMoisture))
		}
	}
	d.AlertMsg = strings.Join(alerts, "; ")

	return d
}

// EnforceMaxWatering forces off any plant whose pump has run past its hard
// deadline and returns their IDs. It is driven by the tick goroutine (wall
// clock), independent of telemetry, so a stuck sensor or stalled edge node
// cannot leave the pump on indefinitely. The caller publishes water_off for
// each returned ID.
func (c *Controller) EnforceMaxWatering(now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var off []string
	for id, on := range c.watering {
		if !on {
			continue
		}
		deadline, ok := c.waterDeadline[id]
		if ok && now.After(deadline) {
			c.watering[id] = false
			delete(c.waterDeadline, id)
			off = append(off, id)
		}
	}
	return off
}

// Reset clears all stored state for a plant. Call this when a device is
// reprovisioned so stale hysteresis state does not carry over.
func (c *Controller) Reset(plantID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.watering, plantID)
	delete(c.cooledUntil, plantID)
	delete(c.waterDeadline, plantID)
}
