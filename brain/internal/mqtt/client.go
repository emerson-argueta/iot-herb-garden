package mqtt

import (
	"fmt"
	"log"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// Publisher is the narrow interface the handlers need from the MQTT client.
// Defining it here (rather than using pahomqtt.Client directly) lets tests
// inject a mock without pulling in the full paho dependency.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token
}

// clientConfig holds the parameters assembled by functional options.
type clientConfig struct {
	broker   string
	clientID string
	username string
	password string
}

// ClientOption is a functional option for [NewClient].
type ClientOption func(*clientConfig)

func WithBroker(addr string) ClientOption {
	return func(c *clientConfig) { c.broker = addr }
}

func WithClientID(id string) ClientOption {
	return func(c *clientConfig) { c.clientID = id }
}

func WithCredentials(username, password string) ClientOption {
	return func(c *clientConfig) {
		c.username = username
		c.password = password
	}
}

// NewClient builds and connects a paho MQTT client from the supplied options.
// The returned pahomqtt.Client satisfies Publisher.
func NewClient(opts ...ClientOption) (pahomqtt.Client, error) {
	cfg := &clientConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	pahoOpts := pahomqtt.NewClientOptions().
		AddBroker(cfg.broker).
		SetClientID(cfg.clientID).
		SetCleanSession(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(_ pahomqtt.Client) {
			log.Printf("[mqtt] connected to %s", cfg.broker)
		}).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
		})

	if cfg.username != "" {
		pahoOpts.SetUsername(cfg.username).SetPassword(cfg.password)
	}

	client := pahomqtt.NewClient(pahoOpts)
	token := client.Connect()
	token.WaitTimeout(10 * time.Second)
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}

	return client, nil
}

// Subscribe registers a handler on a topic. Kept as a free function so callers
// can iterate a list of subscriptions without boilerplate.
func Subscribe(client pahomqtt.Client, topic string, qos byte, handler pahomqtt.MessageHandler) error {
	token := client.Subscribe(topic, qos, handler)
	token.WaitTimeout(5 * time.Second)
	if err := token.Error(); err != nil {
		return fmt.Errorf("subscribe %s: %w", topic, err)
	}
	log.Printf("[mqtt] subscribed → %s", topic)
	return nil
}
