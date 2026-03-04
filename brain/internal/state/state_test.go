package state_test

import (
	"testing"
	"time"

	"github.com/iot-herb-garden/brain/internal/domain"
	"github.com/iot-herb-garden/brain/internal/state"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func tel(plantID string, moisture, temp float64) domain.Telemetry {
	return domain.Telemetry{PlantID: plantID, Moisture: moisture, Temp: temp}
}

func dec(plantID string, watering, moistAlert, tempAlert bool) domain.Decision {
	return domain.Decision{
		PlantID:       plantID,
		Watering:      watering,
		MoistureAlert: moistAlert,
		TempAlert:     tempAlert,
	}
}

// ── BuildStatePayload ─────────────────────────────────────────────────────────

func TestBuildStatePayload_Empty(t *testing.T) {
	s := state.NewStore()
	p := s.BuildStatePayload()
	if len(p.Plants) != 0 {
		t.Errorf("want 0 plants, got %d", len(p.Plants))
	}
	if len(p.UnprovisionedDevices) != 0 {
		t.Errorf("want 0 unprovisioned, got %d", len(p.UnprovisionedDevices))
	}
}

func TestBuildStatePayload_TelemetryWithoutDecision(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(tel("thyme_1", 45, 22))

	p := s.BuildStatePayload()
	if len(p.Plants) != 1 {
		t.Fatalf("want 1 plant, got %d", len(p.Plants))
	}
	ps := p.Plants["thyme_1"]
	if ps.Moisture != 45 || ps.Temp != 22 {
		t.Errorf("wrong sensor values: moisture=%.1f temp=%.1f", ps.Moisture, ps.Temp)
	}
	// Decision fields must be zero-valued.
	if ps.Watering || ps.MoistureAlert || ps.TempAlert {
		t.Error("decision fields should be zero before any decision is stored")
	}
}

func TestBuildStatePayload_JoinsDecision(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(tel("thyme_1", 10, 22))
	s.UpdateDecision(dec("thyme_1", true, true, false))

	ps := s.BuildStatePayload().Plants["thyme_1"]
	if !ps.Watering {
		t.Error("Watering should be true")
	}
	if !ps.MoistureAlert {
		t.Error("MoistureAlert should be true")
	}
}

func TestBuildStatePayload_DisplayName(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(tel("thyme_1", 50, 22))
	s.SetDisplayName("thyme_1", "Thyme")

	ps := s.BuildStatePayload().Plants["thyme_1"]
	if ps.DisplayName != "Thyme" {
		t.Errorf("want DisplayName %q, got %q", "Thyme", ps.DisplayName)
	}
}

func TestBuildStatePayload_LastSeen(t *testing.T) {
	s := state.NewStore()
	before := time.Now().Unix()
	s.UpdateTelemetry(tel("thyme_1", 50, 22))
	after := time.Now().Unix()

	ps := s.BuildStatePayload().Plants["thyme_1"]
	if ps.LastSeen < before || ps.LastSeen > after {
		t.Errorf("LastSeen %d out of range [%d, %d]", ps.LastSeen, before, after)
	}
}

// ── Unprovisioned devices ─────────────────────────────────────────────────────

func TestUnprovisioned_AddRemove(t *testing.T) {
	s := state.NewStore()
	s.AddUnprovisioned("AA:BB:CC")
	s.AddUnprovisioned("DD:EE:FF")

	p := s.BuildStatePayload()
	if len(p.UnprovisionedDevices) != 2 {
		t.Errorf("want 2 unprovisioned, got %d", len(p.UnprovisionedDevices))
	}

	s.RemoveUnprovisioned("AA:BB:CC")
	p = s.BuildStatePayload()
	if len(p.UnprovisionedDevices) != 1 {
		t.Errorf("want 1 unprovisioned after remove, got %d", len(p.UnprovisionedDevices))
	}
	if p.UnprovisionedDevices[0] != "DD:EE:FF" {
		t.Errorf("wrong remaining device: %s", p.UnprovisionedDevices[0])
	}
}

// ── AllDecisions / AllLastSeen (used by alertmanager) ─────────────────────────

func TestAllDecisions_Snapshot(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(tel("p1", 10, 20))
	s.UpdateDecision(dec("p1", true, true, false))

	snap := s.AllDecisions()
	if len(snap) != 1 {
		t.Fatalf("want 1 decision, got %d", len(snap))
	}
	if !snap["p1"].Watering {
		t.Error("Watering should be true in snapshot")
	}
	// Snapshot should be independent of subsequent mutations.
	s.UpdateDecision(dec("p1", false, false, false))
	if !snap["p1"].Watering {
		t.Error("snapshot should not be affected by later updates")
	}
}

func TestAllLastSeen_Snapshot(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(tel("p1", 50, 22))

	snap := s.AllLastSeen()
	if len(snap) != 1 {
		t.Fatalf("want 1 entry, got %d", len(snap))
	}
	if snap["p1"].IsZero() {
		t.Error("last-seen time should not be zero")
	}
}
