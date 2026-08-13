package homeassistant

import (
	"fmt"
	"log"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"gpb/internal/config"
)

const (
	connectTimeout = 15 * time.Second
	publishTimeout = 10 * time.Second
	// atLeastOnce is enough for everything here. Every message is retained and idempotent — the
	// latest state replaces the last — so a duplicate costs nothing and a lost one would be a
	// dashboard stuck on yesterday.
	atLeastOnce = byte(1)
)

// client is the MQTT half, kept apart from the rest of the package so that what gets published
// can be tested without a broker to publish to.
type client struct {
	mqtt mqtt.Client
}

func newClient(settings config.MQTT, availabilityTopic string, onConnect func(), onLost func(error)) transport {
	options := mqtt.NewClientOptions().
		AddBroker(settings.BrokerURL()).
		SetClientID(settings.TopicPrefix).
		SetUsername(settings.Username).
		SetPassword(settings.Password).
		// paho's own connect timeout defaults to twice ours, which left it still dialling a slow
		// broker after we had given up and reported a failure.
		SetConnectTimeout(connectTimeout).
		// The will is what tells Home Assistant the truth when this container is killed rather
		// than stopped: without it every entity would keep showing the last thing it heard.
		SetWill(availabilityTopic, offline, atLeastOnce, true).
		// Auto-reconnect but not connect-retry: retrying the first CONNECT is paho's own affair
		// and it does it silently, so a broker refusing the password reads as one that never
		// answered. The bridge retries instead, and gets to say why each time.
		SetAutoReconnect(true).
		SetMaxReconnectInterval(2 * time.Minute).
		SetCleanSession(true).
		SetOnConnectHandler(func(mqtt.Client) { onConnect() }).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			log.Printf("home assistant: lost the broker: %v", err)
			onLost(err)
		})

	return &client{mqtt: mqtt.NewClient(options)}
}

func (c *client) connect() error {
	token := c.mqtt.Connect()
	if !token.WaitTimeout(connectTimeout) {
		return fmt.Errorf("the broker did not answer within %s", connectTimeout)
	}
	return token.Error()
}

func (c *client) publish(topic string, retained bool, payload []byte) error {
	token := c.mqtt.Publish(topic, atLeastOnce, retained, payload)
	if !token.WaitTimeout(publishTimeout) {
		return fmt.Errorf("the broker did not accept the message within %s", publishTimeout)
	}
	return token.Error()
}

func (c *client) subscribe(topic string, handle func(topic string, payload []byte)) error {
	token := c.mqtt.Subscribe(topic, atLeastOnce, func(_ mqtt.Client, message mqtt.Message) {
		handle(message.Topic(), message.Payload())
	})
	if !token.WaitTimeout(publishTimeout) {
		return fmt.Errorf("the broker did not answer the subscription within %s", publishTimeout)
	}
	return token.Error()
}

func (c *client) disconnect() {
	c.mqtt.Disconnect(uint(publishTimeout.Milliseconds()))
}
