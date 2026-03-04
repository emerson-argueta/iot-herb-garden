// Package alertmanager evaluates plant health and node connectivity on each
// tick and sends de-duplicated, rate-limited notifications via a Notifier.
//
// # Alert kinds
//
//   - KindThreshold  — moisture or temperature outside configured bounds.
//     Stateful: sends on OK→CRITICAL, CRITICAL→OK, and after ReNotifyInterval
//     when CRITICAL persists.
//
//   - KindWatering   — pump activated by the controller.
//     Event-only: sends a one-shot notification each time the pump turns on,
//     subject to ReNotifyInterval debouncing. No "resolved" email when the
//     pump turns off (that is normal operation, not a fault).
//
//   - KindNodeOffline — edge node has not reported within WatchdogTimeout.
//     Stateful: same OK↔CRITICAL model as threshold alerts.
//
// # Anti-spam model
//
// Every (plantID, AlertKind) pair is an independent key in the alertRecord
// map. The map stores the last-known status and the time the last notification
// was sent. A notification fires when:
//
//  1. The status changes (OK→CRITICAL or CRITICAL→OK).
//  2. The status stays CRITICAL and ReNotifyInterval has elapsed since the
//     last send (prevents silence on long-running issues).
//
// # Concurrency
//
// Check() is designed to be called from a single goroutine (the main tick
// loop). It collects all pending sends under a short-held mutex, then
// delivers emails outside the lock so SMTP latency never blocks state reads.
package alertmanager

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/iot-herb-garden/brain/internal/config"
	"github.com/iot-herb-garden/brain/internal/domain"
	"github.com/iot-herb-garden/brain/internal/notifier"
	"github.com/iot-herb-garden/brain/internal/state"
)

// ── Alert taxonomy ────────────────────────────────────────────────────────────

// AlertKind identifies the category of an alert.
type AlertKind string

const (
	KindThreshold   AlertKind = "threshold"
	KindWatering    AlertKind = "watering"
	KindNodeOffline AlertKind = "node_offline"
)

// alertStatus is the two-state model used by stateful alert kinds.
type alertStatus uint8

const (
	statusOK       alertStatus = 0
	statusCritical alertStatus = 1
)

func (s alertStatus) String() string {
	if s == statusCritical {
		return "CRITICAL"
	}
	return "OK"
}

// alertKey uniquely identifies one alert stream.
// A struct key avoids string allocations on every map lookup.
type alertKey struct {
	PlantID string
	Kind    AlertKind
}

// alertRecord is the persisted, per-stream alert state.
type alertRecord struct {
	status   alertStatus
	lastSent time.Time // zero value = never sent
}

// ── Configuration ─────────────────────────────────────────────────────────────

// Config holds operational parameters for the AlertManager.
type Config struct {
	// ReNotifyInterval is how long to wait before re-sending a notification
	// for an alert that remains in the CRITICAL state. Prevents inbox silence
	// on long-running issues. Typical: 4 hours.
	ReNotifyInterval time.Duration

	// WatchdogTimeout is the maximum acceptable silence from an edge node.
	// If no telemetry arrives within this window the node is flagged offline.
	// Typical: 20 minutes (edge nodes report every 15 minutes).
	WatchdogTimeout time.Duration
}

// Option is a functional option for [New].
type Option func(*AlertManager)

// WithClock injects a custom clock. Use this in tests to control time without
// sleeping or relying on wall time.
//
//	var now time.Time
//	am := alertmanager.New(n, cfg, plants, alertmanager.WithClock(func() time.Time { return now }))
func WithClock(fn func() time.Time) Option {
	return func(am *AlertManager) { am.now = fn }
}

// ── AlertManager ──────────────────────────────────────────────────────────────

// AlertManager monitors plant decisions and node heartbeats, maintaining
// per-alert state to prevent notification spam.
type AlertManager struct {
	notifier notifier.Notifier
	cfg      Config
	plants   map[string]config.PlantConfig // read-only; used only for display names

	mu           sync.Mutex
	records      map[alertKey]alertRecord
	prevWatering map[string]bool // pump state seen on last Check call

	now func() time.Time
}

// New creates an AlertManager. plants is the plant config map from config.yaml
// and is used only to resolve display names in email bodies.
func New(n notifier.Notifier, cfg Config, plants map[string]config.PlantConfig, opts ...Option) *AlertManager {
	am := &AlertManager{
		notifier:     n,
		cfg:          cfg,
		plants:       plants,
		records:      make(map[alertKey]alertRecord),
		prevWatering: make(map[string]bool),
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(am)
	}
	return am
}

// Check evaluates all current plant state and node heartbeats against the
// alert rules. It must be called from the tick goroutine (not from MQTT
// message handlers) to avoid blocking message delivery.
func (am *AlertManager) Check(store *state.Store) {
	decisions := store.AllDecisions()
	lastSeens := store.AllLastSeen()
	now := am.now()

	// pendingSend carries everything needed to send one email. Collected under
	// am.mu, then delivered outside the lock so SMTP latency is not a problem.
	type pendingSend struct {
		key     alertKey
		subject string
		body    string
	}
	var sends []pendingSend

	// ── Phase 1: evaluate all rules, collect what needs sending ──────────────
	am.mu.Lock()

	for plantID, d := range decisions {
		// Threshold breach ─────────────────────────────────────────────────────
		key := alertKey{plantID, KindThreshold}
		isCritical := d.MoistureAlert || d.TempAlert
		detail := d.AlertMsg
		if !isCritical {
			detail = "Sensor readings have returned to normal."
		}
		if sub, body, ok := am.evalStateful(now, key, isCritical, detail, plantID); ok {
			sends = append(sends, pendingSend{key, sub, body})
		}

		// Actuator event ───────────────────────────────────────────────────────
		// Detect off→on transition by comparing with the state from the last
		// Check call. Only the rising edge fires a notification; pump-off is
		// normal operation and does not generate a "resolved" alert.
		wasWatering := am.prevWatering[plantID]
		if !wasWatering && d.Watering {
			key := alertKey{plantID, KindWatering}
			detail := am.wateringDetail(plantID, d)
			if sub, body, ok := am.evalOneShot(now, key, detail, plantID); ok {
				sends = append(sends, pendingSend{key, sub, body})
			}
		}
		am.prevWatering[plantID] = d.Watering
	}

	for plantID, lastSeen := range lastSeens {
		// Node offline watchdog ────────────────────────────────────────────────
		key := alertKey{plantID, KindNodeOffline}
		silent := now.Sub(lastSeen)
		isOffline := silent > am.cfg.WatchdogTimeout

		detail := fmt.Sprintf(
			"Node has not reported for %s (last seen: %s ago).",
			am.cfg.WatchdogTimeout, silent.Truncate(time.Second))
		if !isOffline {
			detail = "Node is reporting normally."
		}
		if sub, body, ok := am.evalStateful(now, key, isOffline, detail, plantID); ok {
			sends = append(sends, pendingSend{key, sub, body})
		}
	}

	am.mu.Unlock()

	// ── Phase 2: deliver emails outside the lock ──────────────────────────────
	// lastSent is written optimistically in Phase 1, so a delivery failure
	// means the next retry happens after ReNotifyInterval — this is correct
	// behaviour and avoids spam on SMTP recovery.
	for _, s := range sends {
		if err := am.notifier.Send(s.subject, s.body); err != nil {
			log.Printf("[alertmanager] send failed [%s/%s]: %v",
				s.key.PlantID, s.key.Kind, err)
		} else {
			log.Printf("[alertmanager] alert sent [%s/%s]",
				s.key.PlantID, s.key.Kind)
		}
	}
}

// ── Internal evaluation helpers ───────────────────────────────────────────────

// evalStateful implements the OK↔CRITICAL state machine with re-notify logic.
// It updates the alertRecord unconditionally and returns (subject, body, true)
// only when a notification should be sent.
func (am *AlertManager) evalStateful(
	now time.Time,
	key alertKey,
	isCritical bool,
	detail string,
	plantID string,
) (subject, body string, send bool) {
	rec := am.records[key]
	newStatus := statusOK
	if isCritical {
		newStatus = statusCritical
	}

	switch {
	case newStatus != rec.status:
		// Status transition always fires regardless of time.
		send = true
	case newStatus == statusCritical && now.Sub(rec.lastSent) >= am.cfg.ReNotifyInterval:
		// Sustained CRITICAL: re-notify after the quiet window.
		send = true
	}

	if send {
		subject = am.formatSubject(key.Kind, newStatus, plantID)
		body = am.formatBody(key.Kind, newStatus, detail, plantID, now)
		// lastSent is recorded optimistically before the actual SMTP call so
		// that a second concurrent Check (e.g., if ever parallelised) does not
		// double-send for the same event.
		am.records[key] = alertRecord{status: newStatus, lastSent: now}
	} else {
		// Always persist the latest status so the next transition is detected
		// even when no notification was sent.
		am.records[key] = alertRecord{status: newStatus, lastSent: rec.lastSent}
	}
	return
}

// evalOneShot implements debounced event delivery with no OK/CRITICAL model.
// It fires on the first call and then not again until ReNotifyInterval has
// elapsed. Used for KindWatering where pump-off is not a "resolved" state.
func (am *AlertManager) evalOneShot(
	now time.Time,
	key alertKey,
	detail string,
	plantID string,
) (subject, body string, send bool) {
	rec := am.records[key]
	if rec.lastSent.IsZero() || now.Sub(rec.lastSent) >= am.cfg.ReNotifyInterval {
		send = true
		subject = am.formatSubject(key.Kind, statusCritical, plantID)
		body = am.formatBody(key.Kind, statusCritical, detail, plantID, now)
		am.records[key] = alertRecord{lastSent: now}
	}
	return
}

// ── Email formatting ──────────────────────────────────────────────────────────

func (am *AlertManager) formatSubject(kind AlertKind, status alertStatus, plantID string) string {
	label := map[AlertKind]string{
		KindThreshold:   "Threshold Breach",
		KindWatering:    "Pump Activated",
		KindNodeOffline: "Node Offline",
	}[kind]

	severity := "[CRITICAL]"
	switch {
	case kind == KindWatering:
		severity = "[INFO]"
	case status == statusOK:
		severity = "[RESOLVED]"
	}

	return fmt.Sprintf("%s %s — %s (%s)", severity, label, am.displayName(plantID), plantID)
}

func (am *AlertManager) formatBody(
	kind AlertKind,
	status alertStatus,
	detail string,
	plantID string,
	now time.Time,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plant:   %s (%s)\n", am.displayName(plantID), plantID)
	fmt.Fprintf(&b, "Kind:    %s\n", kindLabel(kind))
	if kind == KindWatering {
		fmt.Fprintf(&b, "Status:  Pump is running\n")
	} else {
		fmt.Fprintf(&b, "Status:  %s\n", status)
	}
	fmt.Fprintf(&b, "Detail:  %s\n", detail)
	fmt.Fprintf(&b, "Time:    %s\n", now.UTC().Format("2006-01-02 15:04:05 UTC"))
	return b.String()
}

func (am *AlertManager) displayName(plantID string) string {
	if p, ok := am.plants[plantID]; ok && p.DisplayName != "" {
		return p.DisplayName
	}
	return plantID
}

// wateringDetail builds the actuator email detail line using the decision's
// AlertMsg (already human-readable from the controller) and the plant config.
func (am *AlertManager) wateringDetail(plantID string, d domain.Decision) string {
	if d.AlertMsg != "" {
		return fmt.Sprintf("Pump activated. Sensor reading: %s", d.AlertMsg)
	}
	if p, ok := am.plants[plantID]; ok {
		return fmt.Sprintf(
			"Pump activated — moisture below %.0f%% minimum threshold.", p.MinMoisture)
	}
	return "Pump activated by controller."
}

func kindLabel(k AlertKind) string {
	switch k {
	case KindThreshold:
		return "Threshold Breach"
	case KindWatering:
		return "Actuator Event"
	case KindNodeOffline:
		return "Node Offline"
	}
	return string(k)
}
