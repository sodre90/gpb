package homeassistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"gpb/internal/config"
)

// fakeBroker stands in for the broker so that everything worth testing here — what is published,
// where, and whether it is published twice — can be tested without one running.
type fakeBroker struct {
	mu           sync.Mutex
	messages     []message
	listeners    map[string]func(topic string, payload []byte)
	refuse       error
	refuseToOpen error
	hungUp       int
	lost         func(error)
}

type message struct {
	topic    string
	retained bool
	payload  string
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{listeners: map[string]func(string, []byte){}}
}

func (f *fakeBroker) connect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refuseToOpen
}

func (f *fakeBroker) publish(topic string, retained bool, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.refuse != nil {
		return f.refuse
	}
	f.messages = append(f.messages, message{topic: topic, retained: retained, payload: string(payload)})
	return nil
}

func (f *fakeBroker) subscribe(topic string, handle func(topic string, payload []byte)) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.listeners[topic] = handle
	return nil
}

func (f *fakeBroker) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hungUp++
}

func (f *fakeBroker) timesHungUp() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hungUp
}

// dropTheConnection plays paho noticing the broker has gone, which is the only way the bridge
// ever hears about it.
func (f *fakeBroker) dropTheConnection(cause error) {
	f.mu.Lock()
	lost := f.lost
	f.mu.Unlock()
	lost(cause)
}

// deliver plays the broker delivering a message on whichever subscription matches, wildcard
// included, the way a real one would.
func (f *fakeBroker) deliver(t *testing.T, topic, payload string) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for pattern, handle := range f.listeners {
		if matches(pattern, topic) {
			handle(topic, []byte(payload))
			return
		}
	}
	t.Fatalf("nothing is listening on %s; subscriptions are %v", topic, f.listeners)
}

func matches(pattern, topic string) bool {
	wanted, got := strings.Split(pattern, "/"), strings.Split(topic, "/")
	if len(wanted) != len(got) {
		return false
	}
	for at, level := range wanted {
		if level != "+" && level != got[at] {
			return false
		}
	}
	return true
}

func (f *fakeBroker) sentTo(topic string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var payloads []string
	for _, sent := range f.messages {
		if sent.topic == topic {
			payloads = append(payloads, sent.payload)
		}
	}
	return payloads
}

type fixedSource struct {
	mu    sync.Mutex
	state State
}

func (s *fixedSource) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *fixedSource) set(state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

type recordedCommands struct {
	syncs      []string
	refreshes  []string
	library    []string
	albumModes map[string]string
	refuse     error
}

func (c *recordedCommands) StartSync(reason string) error {
	c.syncs = append(c.syncs, reason)
	return c.refuse
}

func (c *recordedCommands) RefreshAlbums(reason string) error {
	c.refreshes = append(c.refreshes, reason)
	return c.refuse
}

func (c *recordedCommands) SetLibraryMode(mode string) error {
	c.library = append(c.library, mode)
	return c.refuse
}

func (c *recordedCommands) SetAlbumMode(albumID, mode string) error {
	if c.albumModes == nil {
		c.albumModes = map[string]string{}
	}
	c.albumModes[albumID] = mode
	return c.refuse
}

func testBridge(t *testing.T) (*Bridge, *fakeBroker, *fixedSource, *recordedCommands) {
	t.Helper()

	settings := config.Defaults().MQTT
	settings.Broker = "192.168.1.10"

	bridge, broker, source, commands := bridgeReading(func() config.MQTT { return settings })
	bridge.transport = broker
	return bridge, broker, source, commands
}

// bridgeReading is a bridge whose broker is a fake one, so that Run and everything it decides —
// which settings to use, what to say about them — can be exercised without a broker to dial.
func bridgeReading(settings Settings) (*Bridge, *fakeBroker, *fixedSource, *recordedCommands) {
	source, commands, broker := &fixedSource{}, &recordedCommands{}, newFakeBroker()
	bridge := New(settings, "http://gpb.local:8090", source, commands)
	bridge.dial = func(_ config.MQTT, _ string, _ func(), onLost func(error)) transport {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		broker.lost = onLost
		return broker
	}
	return bridge, broker, source, commands
}

func TestEverythingIsAnnouncedRetainedSoHomeAssistantRebuildsItself(t *testing.T) {
	bridge, broker, _, _ := testBridge(t)
	bridge.announce()

	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, sent := range broker.messages {
		if !sent.retained {
			t.Errorf("%s was published without the retain flag", sent.topic)
		}
	}

	for _, topic := range []string{
		"homeassistant/sensor/gpb/activity/config",
		"homeassistant/binary_sensor/gpb/running/config",
		"homeassistant/sensor/gpb/free_space/config",
		"homeassistant/button/gpb/back_up_now/config",
		"homeassistant/select/gpb/library/config",
	} {
		var found bool
		for _, sent := range broker.messages {
			found = found || sent.topic == topic
		}
		if !found {
			t.Errorf("nothing was announced at %s", topic)
		}
	}
}

// The entities are only one device if every one of them says so, and the unique ids are what let
// Home Assistant keep a history across restarts rather than making new entities each time.
func TestEveryEntityBelongsToOneDeviceAndIsUniquelyIdentified(t *testing.T) {
	bridge, broker, _, _ := testBridge(t)
	bridge.announce()

	seen := map[string]bool{}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, sent := range broker.messages {
		if !strings.HasSuffix(sent.topic, "/config") {
			continue
		}

		var announced struct {
			UniqueID string `json:"unique_id"`
			Device   struct {
				Identifiers []string `json:"identifiers"`
			} `json:"device"`
			Availability string `json:"availability_topic"`
		}
		if err := json.Unmarshal([]byte(sent.payload), &announced); err != nil {
			t.Fatalf("%s carries payload that is not JSON: %v", sent.topic, err)
		}

		switch {
		case announced.UniqueID == "":
			t.Errorf("%s announced an entity with no unique id", sent.topic)
		case seen[announced.UniqueID]:
			t.Errorf("%s reuses the unique id %q", sent.topic, announced.UniqueID)
		case len(announced.Device.Identifiers) != 1 || announced.Device.Identifiers[0] != "gpb":
			t.Errorf("%s belongs to device %v rather than gpb", sent.topic, announced.Device.Identifiers)
		case announced.Availability != "gpb/availability":
			t.Errorf("%s has no availability topic, so it would look alive with gpb stopped", sent.topic)
		}
		seen[announced.UniqueID] = true
	}
}

func TestConnectingSaysOnlineAndStoppingSaysOffline(t *testing.T) {
	bridge, broker, _, _ := testBridge(t)

	bridge.announce()
	bridge.sayGoodbye()

	said := broker.sentTo("gpb/availability")
	if len(said) != 2 || said[0] != online || said[1] != offline {
		t.Errorf("availability went %v, want online then offline", said)
	}
}

// A backup writes files at a few a second for days. Publishing a state that has not changed
// would put that whole rate onto somebody's broker for no information at all.
func TestAStateThatHasNotMovedIsNotPublishedAgain(t *testing.T) {
	bridge, broker, source, _ := testBridge(t)
	source.set(State{Activity: "sync", Running: true, BackedUp: 10})

	bridge.publishIfChanged()
	bridge.publishIfChanged()
	if sent := broker.sentTo("gpb/state"); len(sent) != 1 {
		t.Fatalf("the state was published %d times without changing", len(sent))
	}

	source.set(State{Activity: "sync", Running: true, BackedUp: 11})
	bridge.publishIfChanged()
	if sent := broker.sentTo("gpb/state"); len(sent) != 2 {
		t.Errorf("a state that moved was published %d times, want 2", len(sent))
	}
}

// A publish that failed has not been seen by anyone, so remembering it as published would leave
// the dashboard a message behind until the next thing happened to change.
func TestAStateThatCouldNotBeSentIsSentAgain(t *testing.T) {
	bridge, broker, source, _ := testBridge(t)
	source.set(State{Activity: "sync"})

	broker.refuse = errNoBroker
	bridge.publishIfChanged()
	broker.refuse = nil
	bridge.publishIfChanged()

	if sent := broker.sentTo("gpb/state"); len(sent) != 1 {
		t.Errorf("the state was published %d times, want the one that got through", len(sent))
	}
}

func TestTheStateCarriesWhatTheEntitiesRead(t *testing.T) {
	bridge, broker, source, _ := testBridge(t)
	source.set(State{
		Activity: "sync", Running: true, BackedUp: 96427, Remaining: 25, Progress: 99,
		Downloading: "IMG_2044.MOV", Album: "Mallorca 2026", Failed: 3,
		LastRunAt: "2026-08-12T11:42:58Z", LastOutcome: "ok", FreeGB: 31.4,
		SessionProblem: false, Library: "Everything",
	})
	bridge.publishIfChanged()

	sent := broker.sentTo("gpb/state")
	if len(sent) != 1 {
		t.Fatalf("the state was published %d times, want 1", len(sent))
	}

	var published map[string]any
	if err := json.Unmarshal([]byte(sent[0]), &published); err != nil {
		t.Fatalf("the state is not JSON: %v", err)
	}
	for _, field := range []string{
		"activity", "running", "backed_up", "remaining", "progress", "downloading", "album",
		"failed", "last_run_at", "last_outcome", "free_gb", "session_problem", "library",
	} {
		if _, carried := published[field]; !carried {
			t.Errorf("the state does not carry %q, which an entity reads", field)
		}
	}
}

// Every value template has to name a field the state actually publishes, and nothing but a test
// connects the two: a renamed field would leave the entity showing nothing at all.
func TestEveryEntityReadsAFieldTheStateCarries(t *testing.T) {
	published := map[string]bool{}
	encoded, err := json.Marshal(State{})
	if err != nil {
		t.Fatalf("encoding an empty state: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decoding an empty state: %v", err)
	}
	for field := range fields {
		published[field] = true
	}

	for _, sensor := range sensors {
		if !published[sensor.field] {
			t.Errorf("the %s entity reads value_json.%s, which the state does not carry",
				sensor.id, sensor.field)
		}
	}
	if !published["library"] {
		t.Error("the library selector reads value_json.library, which the state does not carry")
	}
}

func settingsFor(broker string) config.MQTT {
	settings := config.Defaults().MQTT
	settings.Broker = broker
	return settings
}

// Leaving the broker empty is how the whole feature is turned off, so it has to read as off
// rather than as a connection that is failing.
func TestNoBrokerIsSaidWithoutSayingAnythingIsWrong(t *testing.T) {
	bridge, _, _, _ := bridgeReading(func() config.MQTT { return settingsFor("") })

	if changed := bridge.attempt(context.Background()); changed {
		t.Error("an unset broker asked for an immediate retry")
	}
	if standing := bridge.Status(); standing.Connected || standing.Detail != "no broker is set" {
		t.Errorf("an unset broker reads as %+v", standing)
	}
}

// The broker's own reason is the whole value of showing anything: "not Authorized" is the
// difference between a mistyped password and a broker that is not running, and no wording of
// ours can tell them apart.
func TestABrokerThatRefusesUsIsQuotedWordForWord(t *testing.T) {
	bridge, broker, _, _ := bridgeReading(func() config.MQTT { return settingsFor("192.168.1.10") })
	broker.refuseToOpen = errors.New("not Authorized")

	bridge.attempt(context.Background())

	standing := bridge.Status()
	switch {
	case standing.Connected:
		t.Error("a refused connection reads as connected")
	case !strings.Contains(standing.Detail, "not Authorized"):
		t.Errorf("the refusal reads as %q, which does not carry the broker's own words", standing.Detail)
	case standing.Broker != "tcp://192.168.1.10:1883":
		t.Errorf("the refusal is attributed to %q", standing.Broker)
	}
}

// A dial that failed still leaves a client behind, and paho goes on trying to finish it while
// holding the client id the next attempt will ask for. Two sessions under one id is a reconnect
// war: the broker evicts one, the evicted one comes back, and it evicts the other.
func TestADialThatFailedIsStillHungUpOn(t *testing.T) {
	bridge, broker, _, _ := bridgeReading(func() config.MQTT { return settingsFor("192.168.1.10") })
	broker.refuseToOpen = errors.New("not Authorized")

	bridge.attempt(context.Background())

	if hungUp := broker.timesHungUp(); hungUp != 1 {
		t.Errorf("a refused connection was hung up on %d times", hungUp)
	}
}

// paho reconnects on its own, so a connection that drops is only ever heard about through the
// handler. Without it the settings page went on saying "publishing as gpb" for as long as the
// daemon ran, with nothing published since the broker went away.
func TestALostBrokerStopsReadingAsConnectedAndComingBackUndoesIt(t *testing.T) {
	bridge, broker, _, _ := bridgeReading(func() config.MQTT { return settingsFor("192.168.1.10") })

	stopped, stop := context.WithCancel(context.Background())
	stop()
	bridge.attempt(stopped)

	broker.dropTheConnection(errors.New("EOF"))
	standing := bridge.Status()
	switch {
	case standing.Connected:
		t.Error("a dropped connection still reads as connected")
	case !strings.Contains(standing.Detail, "EOF"):
		t.Errorf("the loss reads as %q, which does not say what happened", standing.Detail)
	}

	bridge.announce()
	if standing := bridge.Status(); !standing.Connected {
		t.Errorf("a broker that came back still reads as %+v", standing)
	}
}

func TestAConnectionSaysSoAndNamesTheBroker(t *testing.T) {
	bridge, _, _, _ := bridgeReading(func() config.MQTT { return settingsFor("192.168.1.10") })

	stopped, stop := context.WithCancel(context.Background())
	stop()
	bridge.attempt(stopped)

	if standing := bridge.Status(); !standing.Connected || standing.Broker != "tcp://192.168.1.10:1883" {
		t.Errorf("a connection reads as %+v", standing)
	}
}

// The settings are read on every attempt rather than kept from startup, which is what lets a
// password corrected on the settings page be tried without restarting the daemon.
func TestEachAttemptTakesUpWhateverTheSettingsSayNow(t *testing.T) {
	broker := "192.168.1.10"
	bridge, fake, _, _ := bridgeReading(func() config.MQTT { return settingsFor(broker) })
	fake.refuseToOpen = errors.New("not Authorized")

	bridge.attempt(context.Background())
	broker = "192.168.1.99"
	bridge.attempt(context.Background())

	if standing := bridge.Status(); standing.Broker != "tcp://192.168.1.99:1883" {
		t.Errorf("the second attempt went to %q rather than the broker now configured", standing.Broker)
	}
}

// Saving the settings page has to end the connection it was saved over, or the new credentials
// would sit in the file until something else happened to break the old ones.
func TestReloadingEndsTheConnectionAndAsksToDialAgainAtOnce(t *testing.T) {
	bridge, broker, _, _ := bridgeReading(func() config.MQTT { return settingsFor("192.168.1.10") })

	bridge.Reload()
	if changed := bridge.attempt(context.Background()); !changed {
		t.Fatal("a reload did not ask for another attempt")
	}
	if said := broker.sentTo("gpb/availability"); len(said) == 0 || said[len(said)-1] != offline {
		t.Errorf("availability went %v; a connection being replaced should say offline first", said)
	}
}

// Since is what the page shows as "since 23:24", so it has to mean the connection's own age
// rather than the age of the last check on it.
func TestAConnectionHoldingSteadyKeepsTheTimeItStarted(t *testing.T) {
	bridge, _, _, _ := bridgeReading(func() config.MQTT { return settingsFor("192.168.1.10") })

	stopped, stop := context.WithCancel(context.Background())
	stop()
	bridge.attempt(stopped)
	first := bridge.Status()
	bridge.attempt(stopped)
	second := bridge.Status()

	if !second.Since.Equal(first.Since) {
		t.Errorf("an unchanged connection restarted its clock: %v then %v", first.Since, second.Since)
	}
	if !second.Attempted.After(first.Attempted) {
		t.Error("a second attempt did not move the time it was attempted, so a save would look unanswered")
	}
}
