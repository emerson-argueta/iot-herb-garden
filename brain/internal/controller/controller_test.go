package controller_test

import (
	"strings"
	"testing"
	"time"

	"github.com/iot-herb-garden/brain/internal/controller"
	"github.com/iot-herb-garden/brain/internal/domain"
)

// thymeCfg is the reusable plant config for all tests.
// Thresholds: water on below 20%, water off above 60%, cooldown 20 minutes.
var thymeCfg = domain.PlantConfig{
	MinMoisture:    20,
	MaxMoisture:    60,
	MinTemp:        15,
	MaxTemp:        30,
	CooldownPeriod: 20 * time.Minute,
}

// telemetry is a small helper to reduce repetition in test bodies.
func telemetry(moisture, temp float64) domain.Telemetry {
	return domain.Telemetry{PlantID: "thyme_1", Moisture: moisture, Temp: temp}
}

// newClockCtrl returns a controller wired to a clock whose value is held in
// the returned pointer. Advancing *t in the test advances the controller's
// view of time without sleeping.
func newClockCtrl() (*controller.Controller, *time.Time) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ctrl := controller.New(controller.WithClock(func() time.Time { return now }))
	return ctrl, &now
}

// ── Baseline ─────────────────────────────────────────────────────────────────

func TestEvaluate_NoAction_WhenMoistureInRange(t *testing.T) {
	ctrl := controller.New()
	d := ctrl.Evaluate(thymeCfg, telemetry(40, 22))

	if d.Watering {
		t.Error("pump must be off when moisture is within [MinMoisture, MaxMoisture]")
	}
	if d.WateringChanged {
		t.Error("WateringChanged must be false when nothing transitions")
	}
	if d.MoistureAlert {
		t.Error("MoistureAlert must be false when moisture is in range")
	}
	if d.TempAlert {
		t.Error("TempAlert must be false when temp is in range")
	}
}

// ── Hysteresis: WaterOn ───────────────────────────────────────────────────────

func TestEvaluate_Hysteresis_WaterOn_BelowMinMoisture(t *testing.T) {
	ctrl := controller.New()
	d := ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // 15 < 20

	if !d.Watering {
		t.Error("pump must turn on when moisture is below MinMoisture")
	}
	if !d.WateringChanged {
		t.Error("WateringChanged must be true on the WaterOn transition")
	}
	if !d.MoistureAlert {
		t.Error("MoistureAlert must be set when moisture is below minimum")
	}
}

func TestEvaluate_Hysteresis_NoWaterOn_WhenAlreadyWatering(t *testing.T) {
	ctrl, now := newClockCtrl()

	// First call: dry soil → WaterOn, cooldown opens.
	d1 := ctrl.Evaluate(thymeCfg, telemetry(15, 22))
	if !d1.WateringChanged {
		t.Fatal("prerequisite: expected WaterOn on first call")
	}

	// Advance past cooldown so the moisture check can run again.
	*now = now.Add(thymeCfg.CooldownPeriod + time.Second)

	// Moisture is still below min. Pump is already on — no transition expected.
	d2 := ctrl.Evaluate(thymeCfg, telemetry(15, 22))

	if !d2.Watering {
		t.Error("pump must stay on while moisture is still below MinMoisture")
	}
	if d2.WateringChanged {
		t.Error("WateringChanged must be false: pump was already on")
	}
}

// ── Hysteresis: dead band ────────────────────────────────────────────────────

func TestEvaluate_Hysteresis_PumpStaysOn_InDeadBand(t *testing.T) {
	// The dead band is [MinMoisture, MaxMoisture]. While the pump is on and
	// moisture is inside this range the controller must hold its state — it
	// must NOT send WaterOff just because moisture rose above the lower bound.
	ctrl, now := newClockCtrl()

	ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn
	*now = now.Add(thymeCfg.CooldownPeriod + time.Second)

	// Moisture recovered to 40% — inside the band, pump was on.
	d := ctrl.Evaluate(thymeCfg, telemetry(40, 22))

	if !d.Watering {
		t.Error("pump must stay on: moisture is in dead band, hysteresis prevents WaterOff")
	}
	if d.WateringChanged {
		t.Error("WateringChanged must be false: no transition occurred in dead band")
	}
}

func TestEvaluate_Hysteresis_NoPumpOn_InDeadBand_WhenPumpWasOff(t *testing.T) {
	// Conversely: if the pump is off and moisture is inside the band,
	// the controller must NOT trigger WaterOn. Only below MinMoisture does that.
	ctrl := controller.New()
	d := ctrl.Evaluate(thymeCfg, telemetry(25, 22)) // 25 > 20, pump off

	if d.Watering {
		t.Error("pump must stay off: moisture is above MinMoisture")
	}
	if d.WateringChanged {
		t.Error("WateringChanged must be false when moisture is in range")
	}
}

// ── Hysteresis: WaterOff ──────────────────────────────────────────────────────

func TestEvaluate_Hysteresis_WaterOff_AboveMaxMoisture(t *testing.T) {
	ctrl, now := newClockCtrl()

	ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn
	*now = now.Add(thymeCfg.CooldownPeriod + time.Second)

	d := ctrl.Evaluate(thymeCfg, telemetry(65, 22)) // 65 > 60

	if d.Watering {
		t.Error("pump must turn off when moisture exceeds MaxMoisture")
	}
	if !d.WateringChanged {
		t.Error("WateringChanged must be true on the WaterOff transition")
	}
}

func TestEvaluate_Hysteresis_NoPumpOff_WhenPumpAlreadyOff(t *testing.T) {
	ctrl := controller.New()
	// Over-saturated soil, but pump was never on.
	d := ctrl.Evaluate(thymeCfg, telemetry(80, 22))

	if d.Watering {
		t.Error("pump must stay off: it was already off")
	}
	if d.WateringChanged {
		t.Error("WateringChanged must be false: no transition")
	}
	if !d.MoistureAlert {
		t.Error("MoistureAlert must be set: moisture exceeds MaxMoisture")
	}
}

// ── Cooldown: blocks premature WaterOff ──────────────────────────────────────

func TestEvaluate_Cooldown_BlocksWaterOff_WhileActive(t *testing.T) {
	// Scenario: sensor reads an implausibly fast moisture spike right after
	// WaterOn (e.g., sensor noise or condensation). The cooldown must prevent
	// a premature WaterOff command during soil-absorption time.
	ctrl, now := newClockCtrl()

	ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn

	// 5 minutes in — still inside the 20-minute cooldown.
	*now = now.Add(5 * time.Minute)
	d := ctrl.Evaluate(thymeCfg, telemetry(80, 22)) // moisture shoots up

	if !d.Watering {
		t.Error("pump must stay on: cooldown must prevent WaterOff")
	}
	if d.WateringChanged {
		t.Error("WateringChanged must be false: cooldown blocked the transition")
	}
	if !d.InCooldown {
		t.Error("InCooldown must be true within the cooldown window")
	}
}

// ── Cooldown: blocks WaterOn re-trigger ──────────────────────────────────────

func TestEvaluate_Cooldown_BlocksWaterOnRetrigger_WhileActive(t *testing.T) {
	// If the pump is already on and moisture dips (sensor noise) during the
	// cooldown, the controller must not fire a second WaterOn event.
	ctrl, now := newClockCtrl()

	d1 := ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn
	if !d1.WateringChanged {
		t.Fatal("prerequisite: expected WaterOn")
	}

	*now = now.Add(10 * time.Minute) // still inside cooldown
	d2 := ctrl.Evaluate(thymeCfg, telemetry(5, 22))

	if d2.WateringChanged {
		t.Error("WateringChanged must be false: cooldown blocks re-trigger")
	}
	if !d2.InCooldown {
		t.Error("InCooldown must be true")
	}
}

// ── Cooldown: resumes evaluation after expiry ─────────────────────────────────

func TestEvaluate_Cooldown_WaterOff_FiresAfterExpiry(t *testing.T) {
	ctrl, now := newClockCtrl()

	ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn

	// Jump exactly one second past cooldown expiry.
	*now = now.Add(thymeCfg.CooldownPeriod + time.Second)
	d := ctrl.Evaluate(thymeCfg, telemetry(75, 22)) // well above MaxMoisture

	if d.Watering {
		t.Error("pump must turn off: cooldown elapsed and moisture > MaxMoisture")
	}
	if !d.WateringChanged {
		t.Error("WateringChanged must be true on WaterOff after cooldown expiry")
	}
	if d.InCooldown {
		t.Error("InCooldown must be false: cooldown window has passed")
	}
}

func TestEvaluate_Cooldown_CooldownExpiresAtExactBoundary(t *testing.T) {
	// Verify boundary: at exactly CooldownPeriod the window is still active;
	// at CooldownPeriod+1ns it has expired.
	ctrl, now := newClockCtrl()

	t0 := *now
	ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn at t0

	// At the exact expiry instant the window is still closed (Before uses <).
	*now = t0.Add(thymeCfg.CooldownPeriod)
	dAtBoundary := ctrl.Evaluate(thymeCfg, telemetry(75, 22))
	if !dAtBoundary.InCooldown {
		t.Error("cooldown must still be active at exactly CooldownPeriod")
	}

	// One nanosecond after: window is open.
	*now = t0.Add(thymeCfg.CooldownPeriod + time.Nanosecond)
	dAfterBoundary := ctrl.Evaluate(thymeCfg, telemetry(75, 22))
	if dAfterBoundary.InCooldown {
		t.Error("cooldown must have expired one nanosecond past CooldownPeriod")
	}
	if !dAfterBoundary.WateringChanged {
		t.Error("WaterOff must fire once cooldown expires")
	}
}

// ── Full cycle ────────────────────────────────────────────────────────────────

func TestEvaluate_FullWateringCycle(t *testing.T) {
	// Simulates a realistic end-to-end sequence:
	//   dry soil → WaterOn → cooldown (no commands) → soil saturates → WaterOff
	//   → moisture normalises → no WaterOn (above MinMoisture)
	ctrl, now := newClockCtrl()
	t0 := *now

	steps := []struct {
		label           string
		elapsed         time.Duration
		moisture        float64
		wantWatering    bool
		wantChanged     bool
		wantInCooldown  bool
	}{
		{"1: dry soil triggers WaterOn", 0, 15, true, true, true},
		{"2: moisture rising, in cooldown", 5 * time.Minute, 30, true, false, true},
		{"3: still in cooldown near max", 15 * time.Minute, 55, true, false, true},
		{"4: cooldown elapsed, still below max — pump stays on", 21 * time.Minute, 55, true, false, false},
		{"5: moisture now above max → WaterOff", 22 * time.Minute, 65, false, true, false},
		{"6: moisture back in range, pump was off — no WaterOn", 23 * time.Minute, 40, false, false, false},
	}

	for _, s := range steps {
		t.Run(s.label, func(t *testing.T) {
			*now = t0.Add(s.elapsed)
			d := ctrl.Evaluate(thymeCfg, telemetry(s.moisture, 22))

			if d.Watering != s.wantWatering {
				t.Errorf("Watering: got %v, want %v", d.Watering, s.wantWatering)
			}
			if d.WateringChanged != s.wantChanged {
				t.Errorf("WateringChanged: got %v, want %v", d.WateringChanged, s.wantChanged)
			}
			if d.InCooldown != s.wantInCooldown {
				t.Errorf("InCooldown: got %v, want %v", d.InCooldown, s.wantInCooldown)
			}
		})
	}
}

// ── Temperature alerts ────────────────────────────────────────────────────────

func TestEvaluate_TempAlert_BelowMinTemp(t *testing.T) {
	ctrl := controller.New()
	d := ctrl.Evaluate(thymeCfg, telemetry(40, 10)) // 10 < 15

	if !d.TempAlert {
		t.Error("TempAlert must be set when temp is below MinTemp")
	}
	if !strings.Contains(d.AlertMsg, "minimum") {
		t.Errorf("AlertMsg should mention minimum, got: %q", d.AlertMsg)
	}
}

func TestEvaluate_TempAlert_AboveMaxTemp(t *testing.T) {
	ctrl := controller.New()
	d := ctrl.Evaluate(thymeCfg, telemetry(40, 35)) // 35 > 30

	if !d.TempAlert {
		t.Error("TempAlert must be set when temp is above MaxTemp")
	}
	if !strings.Contains(d.AlertMsg, "maximum") {
		t.Errorf("AlertMsg should mention maximum, got: %q", d.AlertMsg)
	}
}

func TestEvaluate_TempAlert_FiredDuringCooldown(t *testing.T) {
	// Temperature alerts must never be suppressed by cooldown state.
	ctrl, now := newClockCtrl()

	ctrl.Evaluate(thymeCfg, telemetry(15, 22)) // WaterOn

	*now = now.Add(5 * time.Minute) // inside cooldown
	d := ctrl.Evaluate(thymeCfg, telemetry(15, 35))

	if !d.InCooldown {
		t.Error("prerequisite: expected InCooldown=true")
	}
	if !d.TempAlert {
		t.Error("TempAlert must fire even when controller is in cooldown")
	}
}

// ── Plant isolation ───────────────────────────────────────────────────────────

func TestEvaluate_PlantStateIsIsolated(t *testing.T) {
	// The controller must maintain independent state per plant_id.
	ctrl := controller.New()

	dA := ctrl.Evaluate(thymeCfg, domain.Telemetry{PlantID: "plant_a", Moisture: 10, Temp: 22})
	dB := ctrl.Evaluate(thymeCfg, domain.Telemetry{PlantID: "plant_b", Moisture: 40, Temp: 22})

	if !dA.Watering {
		t.Error("plant_a should be watering (dry)")
	}
	if dB.Watering {
		t.Error("plant_b must not be affected by plant_a's state")
	}
	if dB.WateringChanged {
		t.Error("plant_b must show no transition")
	}
}

// ── Reset ─────────────────────────────────────────────────────────────────────

func TestReset_ClearsHysteresisAndCooldown(t *testing.T) {
	ctrl, now := newClockCtrl()

	// Put plant into watering + cooldown state.
	ctrl.Evaluate(thymeCfg, telemetry(15, 22))

	ctrl.Reset("thyme_1")

	// After reset, a dry reading must trigger WaterOn as if fresh.
	// (Cooldown was also cleared, so no cooldown guard either.)
	d := ctrl.Evaluate(thymeCfg, telemetry(15, 22))
	_ = now // keep reference
	if !d.WateringChanged {
		t.Error("WaterOn must fire after Reset clears stale state")
	}
}
