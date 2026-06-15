package alertmanager_test

import (
	"strings"
	"testing"
	"time"

	"github.com/iot-herb-garden/brain/internal/alertmanager"
	"github.com/iot-herb-garden/brain/internal/config"
	"github.com/iot-herb-garden/brain/internal/domain"
	"github.com/iot-herb-garden/brain/internal/state"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

// recorder captures every Send call for assertion.
type recorder struct {
	msgs []string
}

func (r *recorder) Send(subject, body string) error {
	r.msgs = append(r.msgs, subject)
	return nil
}

func (r *recorder) count() int { return len(r.msgs) }

func (r *recorder) lastSubject() string {
	if len(r.msgs) == 0 {
		return ""
	}
	return r.msgs[len(r.msgs)-1]
}

// newTestSetup returns a clock pointer, a recorder, and an AlertManager wired
// together.  plants contains one plant "p1" with MinMoisture=20 MaxMoisture=60.
func newTestSetup() (*time.Time, *recorder, *alertmanager.AlertManager) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	rec := &recorder{}
	plants := &config.Config{Plants: map[string]config.PlantConfig{
		"p1": {DisplayName: "Plant One", MinMoisture: 20, MaxMoisture: 60},
	}}
	am := alertmanager.New(rec, alertmanager.Config{
		ReNotifyInterval: 4 * time.Hour,
		WatchdogTimeout:  20 * time.Minute,
	}, plants, alertmanager.WithClock(func() time.Time { return now }))
	return &now, rec, am
}


// ── Threshold alerts ──────────────────────────────────────────────────────────

func TestThreshold_FirstCritical(t *testing.T) {
	now, rec, am := newTestSetup()
	_ = now

	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 10, Temp: 22})
	s.UpdateDecision(domain.Decision{
		PlantID: "p1", MoistureAlert: true, AlertMsg: "Moisture 10% below 20% minimum",
	})

	am.Check(s)
	if rec.count() != 1 {
		t.Fatalf("want 1 notification, got %d", rec.count())
	}
	if !strings.Contains(rec.lastSubject(), "CRITICAL") {
		t.Errorf("expected CRITICAL in subject, got %q", rec.lastSubject())
	}
}

func TestThreshold_NoSpamOnRecheck(t *testing.T) {
	now, rec, am := newTestSetup()
	_ = now

	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 10, Temp: 22})
	s.UpdateDecision(domain.Decision{PlantID: "p1", MoistureAlert: true})

	am.Check(s) // fires CRITICAL
	am.Check(s) // same state, within re-notify window — must not re-send
	if rec.count() != 1 {
		t.Errorf("want 1 notification (no spam), got %d", rec.count())
	}
}

func TestThreshold_RenotifyAfterInterval(t *testing.T) {
	now, rec, am := newTestSetup()

	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 10, Temp: 22})
	s.UpdateDecision(domain.Decision{PlantID: "p1", MoistureAlert: true})

	am.Check(s) // fires CRITICAL (1)

	*now = now.Add(4 * time.Hour) // exactly at ReNotifyInterval boundary
	am.Check(s)                   // still critical, interval elapsed — fires again (2)
	if rec.count() != 2 {
		t.Errorf("want 2 notifications (re-notify), got %d", rec.count())
	}
}

func TestThreshold_Resolved(t *testing.T) {
	now, rec, am := newTestSetup()
	_ = now

	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 10, Temp: 22})
	s.UpdateDecision(domain.Decision{PlantID: "p1", MoistureAlert: true})
	am.Check(s) // CRITICAL (1)

	// Moisture returns to normal.
	s.UpdateDecision(domain.Decision{PlantID: "p1"})
	am.Check(s) // RESOLVED (2)
	if rec.count() != 2 {
		t.Fatalf("want 2 notifications, got %d", rec.count())
	}
	if !strings.Contains(rec.lastSubject(), "RESOLVED") {
		t.Errorf("expected RESOLVED in subject, got %q", rec.lastSubject())
	}
}

// ── Watering one-shot ─────────────────────────────────────────────────────────

func TestWatering_OneShot(t *testing.T) {
	now, rec, am := newTestSetup()
	_ = now

	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 10, Temp: 22})

	// Pump off → on transition.
	s.UpdateDecision(domain.Decision{PlantID: "p1"})
	am.Check(s) // pump was off last call (zero value); now off — no event

	s.UpdateDecision(domain.Decision{PlantID: "p1", Watering: true})
	am.Check(s) // off→on transition fires (1)
	if rec.count() != 1 {
		t.Fatalf("want 1 notification, got %d", rec.count())
	}

	// Second consecutive check with pump still on — no re-send within interval.
	am.Check(s)
	if rec.count() != 1 {
		t.Errorf("want still 1 notification (no re-send), got %d", rec.count())
	}
}

func TestWatering_NoPumpOffNotification(t *testing.T) {
	now, rec, am := newTestSetup()
	_ = now

	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 10, Temp: 22})

	s.UpdateDecision(domain.Decision{PlantID: "p1", Watering: true})
	am.Check(s) // fires on pump-on (1)

	s.UpdateDecision(domain.Decision{PlantID: "p1", Watering: false})
	am.Check(s) // pump-off is normal — no new notification
	if rec.count() != 1 {
		t.Errorf("want 1 total notification (no pump-off alert), got %d", rec.count())
	}
}

// ── Node offline watchdog ─────────────────────────────────────────────────────

// newWatchdogSetup returns a clock variable and an AlertManager whose clock is
// seeded from the Store's real lastSeen time for "p1", avoiding a mismatch
// between Store's time.Now() calls and the injected alertmanager clock.
func newWatchdogSetup(s *state.Store) (*time.Time, *recorder, *alertmanager.AlertManager) {
	seenAt := s.AllLastSeen()["p1"]
	clk := seenAt
	rec := &recorder{}
	plants := &config.Config{Plants: map[string]config.PlantConfig{
		"p1": {DisplayName: "Plant One"},
	}}
	am := alertmanager.New(rec, alertmanager.Config{
		ReNotifyInterval: 4 * time.Hour,
		WatchdogTimeout:  20 * time.Minute,
	}, plants, alertmanager.WithClock(func() time.Time { return clk }))
	return &clk, rec, am
}

func TestNodeOffline_FreshTelemetryIsOnline(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 50, Temp: 22})
	s.UpdateDecision(domain.Decision{PlantID: "p1"})

	_, rec, am := newWatchdogSetup(s)
	am.Check(s)
	if rec.count() != 0 {
		t.Errorf("want 0 notifications for online node, got %d: %v", rec.count(), rec.msgs)
	}
}

func TestNodeOffline_AlertWhenSilent(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 50, Temp: 22})
	s.UpdateDecision(domain.Decision{PlantID: "p1"})

	clk, rec, am := newWatchdogSetup(s)

	// Advance the injected clock past WatchdogTimeout without new telemetry.
	*clk = clk.Add(21 * time.Minute)
	am.Check(s)
	if rec.count() < 1 || !strings.Contains(rec.lastSubject(), "Offline") {
		t.Fatalf("expected offline alert, got %d msgs: %v", rec.count(), rec.msgs)
	}
}

func TestNodeOffline_Resolved(t *testing.T) {
	s := state.NewStore()
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 50, Temp: 22})
	s.UpdateDecision(domain.Decision{PlantID: "p1"})

	clk, rec, am := newWatchdogSetup(s)

	// Go offline.
	*clk = clk.Add(21 * time.Minute)
	am.Check(s) // CRITICAL
	if !strings.Contains(rec.lastSubject(), "Offline") {
		t.Fatalf("expected offline alert first: %v", rec.msgs)
	}

	// New telemetry arrives — node is back. Re-seed clock to match new lastSeen.
	s.UpdateTelemetry(domain.Telemetry{PlantID: "p1", Moisture: 50, Temp: 22})
	*clk = s.AllLastSeen()["p1"]
	am.Check(s) // RESOLVED
	if !strings.Contains(rec.lastSubject(), "RESOLVED") {
		t.Errorf("expected RESOLVED, got %q", rec.lastSubject())
	}
}
