package homeassistant

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// Commands is what Home Assistant is allowed to ask for. It is deliberately the same handful of
// things the web UI's own buttons do, and they go through the same runner and store underneath,
// so the one-run-at-a-time rule holds however the ask arrived.
type Commands interface {
	StartSync(reason string) error
	RefreshAlbums(reason string) error
	SetLibraryMode(mode string) error
	SetAlbumMode(albumID, mode string) error
}

const askedFromHomeAssistant = "asked from Home Assistant"

type button struct {
	id   string
	name string
	icon string
	do   func(Commands) error
}

var buttons = []button{
	{id: "back_up_now", name: "Back up now", icon: "mdi:cloud-upload",
		do: func(c Commands) error { return c.StartSync(askedFromHomeAssistant) }},
	{id: "refresh_albums", name: "Refresh album list", icon: "mdi:refresh",
		do: func(c Commands) error { return c.RefreshAlbums(askedFromHomeAssistant) }},
}

// libraryChoices are the words the web UI uses for the same setting, so the two do not appear to
// offer different things. The store's own words are on the right.
var libraryChoices = map[string]string{
	"No":         "none",
	"Everything": "all",
}

func (b *Bridge) commandTopic(id string) string {
	return b.settings.TopicPrefix + "/command/" + id
}

// albumModeTopic is control without an entity for each album. There are two hundred albums here
// and picking them is a decision made by looking at photographs, so Home Assistant gets a topic
// an automation can publish to rather than two hundred selectors nobody wants on a dashboard.
func (b *Bridge) albumModeTopic(albumID string) string {
	return fmt.Sprintf("%s/album/%s/mode/set", b.settings.TopicPrefix, albumID)
}

func (b *Bridge) announceCommands() {
	if b.commands == nil {
		return
	}

	for _, press := range buttons {
		payload, err := json.Marshal(map[string]any{
			"name":               press.name,
			"unique_id":          b.nodeID + "_" + press.id,
			"command_topic":      b.commandTopic(press.id),
			"availability_topic": b.availabilityTopic(),
			"icon":               press.icon,
			"device":             b.device(),
		})
		if err != nil {
			log.Printf("home assistant: could not describe the %s button: %v", press.id, err)
			continue
		}
		b.send(fmt.Sprintf("%s/button/%s/%s/config", b.settings.DiscoveryPrefix, b.nodeID, press.id), payload)
	}

	payload, err := json.Marshal(map[string]any{
		"name":               "Whole library",
		"unique_id":          b.nodeID + "_library",
		"command_topic":      b.commandTopic("library"),
		"state_topic":        b.stateTopic(),
		"value_template":     "{{ value_json.library }}",
		"options":            []string{"No", "Everything"},
		"availability_topic": b.availabilityTopic(),
		"icon":               "mdi:image-multiple",
		"device":             b.device(),
	})
	if err != nil {
		log.Printf("home assistant: could not describe the library selector: %v", err)
		return
	}
	b.send(fmt.Sprintf("%s/select/%s/library/config", b.settings.DiscoveryPrefix, b.nodeID), payload)
}

func (b *Bridge) listen() {
	if b.commands == nil {
		return
	}

	for _, topic := range []string{
		b.settings.TopicPrefix + "/command/+",
		b.albumModeTopic("+"),
	} {
		if err := b.transport.subscribe(topic, b.obey); err != nil {
			log.Printf("home assistant: could not listen on %s: %v", topic, err)
		}
	}
}

// obey runs one command from the broker. Anything it cannot make sense of is logged and dropped:
// this is a topic on somebody's home network, and a malformed message is not a reason to take a
// backup down.
func (b *Bridge) obey(topic string, payload []byte) {
	asked := strings.TrimSpace(string(payload))

	if err := b.carryOut(topic, asked); err != nil {
		log.Printf("home assistant: %s: %v", topic, err)
	}
}

func (b *Bridge) carryOut(topic, asked string) error {
	if albumID, ok := b.albumFromTopic(topic); ok {
		return b.commands.SetAlbumMode(albumID, asked)
	}

	command, ok := strings.CutPrefix(topic, b.settings.TopicPrefix+"/command/")
	if !ok {
		return fmt.Errorf("nothing here answers to that topic")
	}
	if command == "library" {
		mode, known := libraryChoices[asked]
		if !known {
			return fmt.Errorf("%q is not one of No or Everything", asked)
		}
		return b.commands.SetLibraryMode(mode)
	}
	for _, press := range buttons {
		if press.id == command {
			return press.do(b.commands)
		}
	}
	return fmt.Errorf("nothing here answers to that command")
}

func (b *Bridge) albumFromTopic(topic string) (string, bool) {
	rest, ok := strings.CutPrefix(topic, b.settings.TopicPrefix+"/album/")
	if !ok {
		return "", false
	}
	albumID, ok := strings.CutSuffix(rest, "/mode/set")
	if !ok || albumID == "" || strings.Contains(albumID, "/") {
		return "", false
	}
	return albumID, true
}
