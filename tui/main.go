package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

const topicState = "garden/state"

func main() {
	broker := flag.String("broker", "tcp://localhost:1883", "MQTT broker address")
	clientID := flag.String("id", "herb-tui", "MQTT client ID")
	debug := flag.Bool("debug", false, "enable MQTT debug logging")
	flag.Parse()

	// Discard all paho logging by default; enable with --debug.
	discard := log.New(io.Discard, "", 0)
	pahomqtt.ERROR = discard
	pahomqtt.WARN = discard
	pahomqtt.CRITICAL = discard
	pahomqtt.DEBUG = discard
	if *debug {
		pahomqtt.ERROR = log.New(os.Stderr, "[mqtt] ERROR ", 0)
		pahomqtt.WARN = log.New(os.Stderr, "[mqtt] WARN  ", 0)
		pahomqtt.CRITICAL = log.New(os.Stderr, "[mqtt] CRIT  ", 0)
		pahomqtt.DEBUG = log.New(os.Stdout, "[mqtt] DEBUG ", 0)
	}

	// Unbuffered channel: MQTT handler blocks until the tea.Cmd reads it.
	stateCh := make(chan StatePayload)
	// Buffered size 1: connect/disconnect events; non-blocking send so handlers
	// never stall the paho goroutine if the model hasn't consumed the last event.
	connCh := make(chan bool, 1)

	sendConn := func(v bool) {
		select {
		case connCh <- v:
		default:
		}
	}

	opts := pahomqtt.NewClientOptions().
		AddBroker(*broker).
		SetClientID(*clientID).
		SetAutoReconnect(true).
		SetCleanSession(true).
		SetOnConnectHandler(func(c pahomqtt.Client) {
			sendConn(true)
			c.Subscribe(topicState, 1, func(_ pahomqtt.Client, msg pahomqtt.Message) {
				var p StatePayload
				if err := json.Unmarshal(msg.Payload(), &p); err != nil {
					return
				}
				stateCh <- p
			})
		}).
		SetConnectionLostHandler(func(_ pahomqtt.Client, _ error) {
			sendConn(false)
		})

	client := pahomqtt.NewClient(opts)
	if tok := client.Connect(); tok.Wait() && tok.Error() != nil {
		fmt.Fprintf(os.Stderr, "MQTT connect: %v\n", tok.Error())
		os.Exit(1)
	}
	defer client.Disconnect(500)

	p := tea.NewProgram(newModel(stateCh, connCh, client), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
}
