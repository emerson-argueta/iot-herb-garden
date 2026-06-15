package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/iot-herb-garden/brain/internal/alertmanager"
	"github.com/iot-herb-garden/brain/internal/config"
	"github.com/iot-herb-garden/brain/internal/controller"
	mqttclient "github.com/iot-herb-garden/brain/internal/mqtt"
	"github.com/iot-herb-garden/brain/internal/notifier"
	"github.com/iot-herb-garden/brain/internal/state"
)

const (
	topicTelemetry = "garden/telemetry"
	topicSetup     = "garden/setup"
	topicAdmin     = "garden/admin"
	topicState     = "garden/state"
	tickInterval   = 5 * time.Second
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("[brain] starting herb garden brain daemon")

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	log.Printf("[brain] loaded config: %d plant(s) defined", len(cfg.Plants))

	store := state.NewStore()
	ctrl := controller.New()
	alerts := buildAlertManager(cfg)

	client, err := mqttclient.NewClient(
		mqttclient.WithBroker(cfg.MQTT.Broker),
		mqttclient.WithClientID(cfg.MQTT.ClientID),
		mqttclient.WithCredentials(cfg.MQTT.Username, cfg.MQTT.Password),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	defer client.Disconnect(500)

	subs := []struct {
		topic   string
		handler func() error
	}{
		{
			topic: topicTelemetry,
			handler: func() error {
				return mqttclient.Subscribe(client, topicTelemetry, 1,
					mqttclient.HandleTelemetry(ctrl, store, cfg, client))
			},
		},
		{
			topic: topicSetup,
			handler: func() error {
				return mqttclient.Subscribe(client, topicSetup, 1,
					mqttclient.HandleSetup(store))
			},
		},
		{
			topic: topicAdmin,
			handler: func() error {
				return mqttclient.Subscribe(client, topicAdmin, 1,
					mqttclient.HandleAdmin(client, store, ctrl, cfg, *cfgPath))
			},
		},
	}

	for _, s := range subs {
		if err := s.handler(); err != nil {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			os.Exit(1)
		}
	}

	// Fail-safe to off: the brain's in-memory pump state does not survive a
	// restart, so on boot we cannot know which pumps an edge node left running.
	// Command every known plant off and let telemetry re-establish real state.
	failSafeOff(client, cfg)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			publishState(client, store)
			// Safety backstop: force off any pump that has run past its hard
			// deadline. Tick-driven so a stalled edge node cannot keep it on.
			for _, id := range ctrl.EnforceMaxWatering(time.Now()) {
				log.Printf("[safety] max watering time exceeded for %s — forcing off", id)
				mqttclient.PublishCommand(client, id, "water_off")
			}
			// AlertManager runs in the same tick goroutine after state is
			// published. It never blocks the MQTT message handler goroutines.
			alerts.Check(store)
		}
	}()

	log.Println("[brain] running — press Ctrl+C to stop")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[brain] shutting down")
}

// buildAlertManager constructs the AlertManager from config, selecting a real
// EmailNotifier when notifications are enabled or a NopNotifier otherwise.
func buildAlertManager(cfg *config.Config) *alertmanager.AlertManager {
	n := cfg.Notifications

	var notif notifier.Notifier
	if n.Enabled {
		notif = notifier.NewEmailNotifier(notifier.EmailConfig{
			Host:      n.SMTPHost,
			Port:      n.SMTPPort,
			Username:  n.Username,
			Password:  n.Password,
			From:      n.From,
			Recipient: n.Recipient,
		})
		log.Printf("[brain] notifications enabled → %s", n.Recipient)
	} else {
		notif = notifier.NopNotifier{}
		log.Println("[brain] notifications disabled (set notifications.enabled: true to activate)")
	}

	return alertmanager.New(notif, alertmanager.Config{
		ReNotifyInterval: n.ReNotifyInterval(),
		WatchdogTimeout:  n.WatchdogTimeout(),
	}, cfg)
}

// failSafeOff commands every configured plant's pump off at startup. The
// brain's pump state is in-memory only, so after a restart it has no record of
// what an edge node left running; defaulting everything to off prevents a pump
// from being stuck on across a brain crash until telemetry resumes.
func failSafeOff(client pahomqtt.Client, cfg *config.Config) {
	ids := cfg.PlantIDs()
	for _, id := range ids {
		mqttclient.PublishCommand(client, id, "water_off")
	}
	if len(ids) > 0 {
		log.Printf("[startup] fail-safe: sent water_off to %d plant(s)", len(ids))
	}
}

// publishState assembles the master snapshot from the store and publishes it.
// No evaluation logic lives here — all enrichment happens in HandleTelemetry.
func publishState(client pahomqtt.Client, store *state.Store) {
	payload := store.BuildStatePayload()

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[tick] marshal error: %v", err)
		return
	}

	token := client.Publish(topicState, 1, true, data)
	token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("[tick] publish error: %v", err)
		return
	}

	log.Printf("[tick] published state: %d plant(s), %d unprovisioned",
		len(payload.Plants), len(payload.UnprovisionedDevices))
}
