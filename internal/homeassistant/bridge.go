package homeassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"gpb/internal/config"
)

// sampleInterval is how often the bridge asks what is happening. Slower than the web UI, which
// only asks while somebody has a page open: this asks for as long as the daemon runs, and each
// sample counts rows in a database a backup is busy writing to. A dashboard is not watched by
// the second anyway.
const sampleInterval = 5 * time.Second

// retryInterval is how long the bridge waits before dialling a broker that would not have it.
const retryInterval = 30 * time.Second

const (
	online  = "online"
	offline = "offline"
)

// transport is the broker as this package needs it. The MQTT client sits behind it so that the
// topics, the payloads and the decision of when to publish can all be tested without a broker —
// which is the whole of this package except the twenty lines that dial one.
type transport interface {
	connect() error
	publish(topic string, retained bool, payload []byte) error
	subscribe(topic string, handle func(topic string, payload []byte)) error
	disconnect()
}

// Settings is read afresh on every attempt rather than kept from startup, so a broker named or a
// password corrected on the settings page is dialled without restarting the daemon.
type Settings func() config.MQTT

// Status is how the bridge is getting on, in the words the settings page shows. Detail carries
// the broker's own reason for a refusal — "not Authorized" is the difference between a wrong
// password and an unplugged broker, and is worth more than any wording of ours.
type Status struct {
	Broker    string
	Connected bool
	Detail    string
	Since     time.Time
	Attempted time.Time
}

// Bridge is one gpb on one broker. It publishes retained messages throughout: discovery so that
// Home Assistant rebuilds its entities after a restart of either end, and state so that those
// entities have values to show the moment they exist rather than after the next change.
//
// settings, nodeID and transport belong to whichever goroutine is running an attempt, and are
// only written between connections — no handler of a live client can be looking at them.
type Bridge struct {
	current     Settings
	externalURL string
	source      Source
	commands    Commands
	dial        func(settings config.MQTT, availabilityTopic string, onConnect func(), onLost func(error)) transport
	reload      chan struct{}

	settings  config.MQTT
	nodeID    string
	transport transport

	publishMu sync.Mutex
	published State
	sent      bool

	statusMu sync.Mutex
	status   Status
}

func New(settings Settings, externalURL string, source Source, commands Commands) *Bridge {
	inUse := settings()
	return &Bridge{
		current:     settings,
		externalURL: externalURL,
		source:      source,
		commands:    commands,
		dial:        newClient,
		reload:      make(chan struct{}, 1),
		settings:    inUse,
		nodeID:      inUse.TopicPrefix,
	}
}

func (b *Bridge) stateTopic() string        { return b.settings.TopicPrefix + "/state" }
func (b *Bridge) availabilityTopic() string { return b.settings.TopicPrefix + "/availability" }

// Run publishes until the context ends, and keeps trying for as long as that takes. A broker
// that is down, or one whose password has yet to be typed into the settings page, is not a
// reason to stop for good: the daemon outlives both, and a message saying why is worth more
// every half minute than once at startup.
func (b *Bridge) Run(ctx context.Context) {
	for {
		if b.attempt(ctx) {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-b.reload:
		case <-time.After(retryInterval):
		}
	}
}

// Reload asks for the settings to be read again and the broker dialled with them, which is what
// makes saving the settings page the test of what was typed into it.
func (b *Bridge) Reload() {
	select {
	case b.reload <- struct{}{}:
	default:
	}
}

func (b *Bridge) Status() Status {
	b.statusMu.Lock()
	defer b.statusMu.Unlock()
	return b.status
}

// attempt takes up whatever the settings say now and stays connected until the context ends or
// they change. It reports whether it was the settings changing, since that asks for the next
// attempt straight away rather than after the usual wait.
func (b *Bridge) attempt(ctx context.Context) (settingsChanged bool) {
	b.settings = b.current()
	b.nodeID = b.settings.TopicPrefix

	if !b.settings.Enabled() {
		b.note(Status{Detail: "no broker is set"})
		return false
	}

	changed, err := b.connectAndServe(ctx)
	if err != nil {
		b.note(Status{Broker: b.settings.BrokerURL(), Detail: err.Error()})
		log.Printf("home assistant: %v; trying again in %s", err, retryInterval)
	}
	return changed
}

func (b *Bridge) connectAndServe(ctx context.Context) (settingsChanged bool, err error) {
	broker := b.settings.BrokerURL()
	link := b.dial(b.settings, b.availabilityTopic(), b.announce, func(cause error) {
		b.note(Status{Broker: broker, Detail: "lost the broker: " + cause.Error()})
	})
	b.transport = link

	// Hung up before the connect is attempted, not after it succeeds. A dial that timed out is
	// still a client, and paho keeps trying to finish it — so leaving this until the happy path
	// abandoned one holding the client id, which the next attempt then took for itself. Brokers
	// evict the older session on a duplicate id, and both halves reconnect on eviction.
	defer link.disconnect()

	if err := link.connect(); err != nil {
		return false, fmt.Errorf("connecting to %s: %w", broker, err)
	}

	log.Printf("home assistant: publishing to %s as %s", broker, b.settings.TopicPrefix)
	b.noteConnected()
	return b.serve(ctx), nil
}

func (b *Bridge) noteConnected() {
	b.note(Status{
		Broker:    b.settings.BrokerURL(),
		Connected: true,
		Detail:    "publishing as " + b.settings.TopicPrefix,
	})
}

func (b *Bridge) serve(ctx context.Context) (settingsChanged bool) {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			b.sayGoodbye()
			return false
		case <-b.reload:
			b.sayGoodbye()
			return true
		case <-ticker.C:
			b.publishIfChanged()
		}
	}
}

// note keeps Since at the moment the state last changed rather than the moment it was last
// confirmed, so a connection holding steady says how long it has held. Attempted moves every
// time, which is how the settings page knows its save has been answered.
func (b *Bridge) note(status Status) {
	b.statusMu.Lock()
	defer b.statusMu.Unlock()

	status.Since = b.status.Since
	if status.Connected != b.status.Connected || status.Detail != b.status.Detail || status.Broker != b.status.Broker {
		status.Since = time.Now()
	}
	status.Attempted = time.Now()
	b.status = status
}

// announce runs on every connection, not only the first. The last will marks this gpb offline
// the moment the broker notices it has gone, so coming back has to say so again — and a broker
// that lost its retained messages has to be told everything a second time.
//
// It is also where a reconnection is noticed at all. paho does its own reconnecting, so the
// only thing that hears about one is this handler; without the note here, a broker that came
// back would go on being reported as lost for as long as the daemon ran.
func (b *Bridge) announce() {
	b.noteConnected()

	for _, sensor := range sensors {
		payload, err := b.discoveryPayload(sensor)
		if err != nil {
			log.Printf("home assistant: could not describe the %s entity: %v", sensor.id, err)
			continue
		}
		b.send(b.discoveryTopic(sensor), payload)
	}
	b.announceCommands()

	b.send(b.availabilityTopic(), []byte(online))
	b.forgetWhatWasPublished()
	b.publishIfChanged()
	b.listen()
}

// sayGoodbye is the orderly version of the last will. A container that is being restarted has
// time to say so, and saying it here means Home Assistant shows the entities as unavailable
// immediately rather than after the broker's keepalive has run out.
func (b *Bridge) sayGoodbye() {
	b.send(b.availabilityTopic(), []byte(offline))
}

// publishIfChanged is called both by the sampling loop and by a reconnection announcing itself,
// which are different goroutines — hence a lock around what was last published, though never
// around the publishing itself: the broker may take seconds to answer, and the page asking how
// the connection is getting on should not wait for it.
func (b *Bridge) publishIfChanged() {
	state := b.source.Snapshot()
	if b.alreadyPublished(state) {
		return
	}

	payload, err := json.Marshal(state)
	if err != nil {
		log.Printf("home assistant: could not encode the state: %v", err)
		return
	}
	if b.send(b.stateTopic(), payload) {
		b.remember(state)
	}
}

func (b *Bridge) alreadyPublished(state State) bool {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	return b.sent && state == b.published
}

func (b *Bridge) remember(state State) {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	b.published, b.sent = state, true
}

func (b *Bridge) forgetWhatWasPublished() {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	b.sent = false
}

func (b *Bridge) send(topic string, payload []byte) bool {
	if err := b.transport.publish(topic, true, payload); err != nil {
		log.Printf("home assistant: publishing to %s: %v", topic, err)
		return false
	}
	return true
}
