package homeassistant

import (
	"encoding/json"
	"fmt"
)

// entity is one thing Home Assistant will show. They all read the same retained state message and
// pick their own field out of it, which is why there is one state topic rather than a dozen: a
// backup that moves publishes once, and every entity moves with it.
type entity struct {
	component   string
	id          string
	name        string
	field       string
	deviceClass string
	unit        string
	stateClass  string
	icon        string
}

// sensors is the whole dashboard. The names are what a person reads on a card, so they say what
// the thing is rather than what the field is called.
var sensors = []entity{
	{component: "sensor", id: "activity", name: "Activity", field: "activity", icon: "mdi:cloud-download"},
	{component: "binary_sensor", id: "running", name: "Backing up", field: "running", deviceClass: "running"},
	{component: "sensor", id: "backed_up", name: "Backed up", field: "backed_up", stateClass: "measurement", icon: "mdi:image-multiple"},
	{component: "sensor", id: "remaining", name: "Left to back up", field: "remaining", stateClass: "measurement", icon: "mdi:image-multiple-outline"},
	{component: "sensor", id: "progress", name: "Progress", field: "progress", unit: "%", stateClass: "measurement", icon: "mdi:progress-download"},
	{component: "sensor", id: "downloading", name: "Downloading", field: "downloading", icon: "mdi:file-download"},
	{component: "sensor", id: "album", name: "Album", field: "album", icon: "mdi:folder-image"},
	{component: "sensor", id: "failed", name: "Failed items", field: "failed", stateClass: "measurement", icon: "mdi:alert-circle-outline"},
	{component: "sensor", id: "last_run_at", name: "Last run", field: "last_run_at", deviceClass: "timestamp"},
	{component: "sensor", id: "last_outcome", name: "Last outcome", field: "last_outcome", icon: "mdi:history"},
	{component: "sensor", id: "free_space", name: "Free space", field: "free_gb", deviceClass: "data_size", unit: "GB", stateClass: "measurement"},
	{component: "binary_sensor", id: "google_session", name: "Google session needs attention", field: "session_problem", deviceClass: "problem"},
}

// discoveryTopic is where Home Assistant looks. The node id keeps every topic this program writes
// under one place on the broker, so what it published can be found — and removed — by hand.
func (b *Bridge) discoveryTopic(e entity) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", b.settings.DiscoveryPrefix, e.component, b.nodeID, e.id)
}

func (b *Bridge) discoveryPayload(e entity) ([]byte, error) {
	payload := map[string]any{
		"name":               e.name,
		"unique_id":          b.nodeID + "_" + e.id,
		"state_topic":        b.stateTopic(),
		"value_template":     valueTemplate(e),
		"availability_topic": b.availabilityTopic(),
		"device":             b.device(),
	}
	for key, value := range map[string]string{
		"device_class": e.deviceClass,
		"state_class":  e.stateClass,
		"icon":         e.icon,
	} {
		if value != "" {
			payload[key] = value
		}
	}
	if e.unit != "" {
		payload["unit_of_measurement"] = e.unit
	}
	if e.component == "binary_sensor" {
		payload["payload_on"], payload["payload_off"] = "ON", "OFF"
	}
	return json.Marshal(payload)
}

// valueTemplate is how one shared message becomes a dozen entities. A binary sensor is the odd
// one out: Home Assistant wants the words ON and OFF, not the true and false the JSON holds.
func valueTemplate(e entity) string {
	if e.component != "binary_sensor" {
		return fmt.Sprintf("{{ value_json.%s }}", e.field)
	}
	return fmt.Sprintf("{{ 'ON' if value_json.%s else 'OFF' }}", e.field)
}

// device is what makes these one thing in Home Assistant rather than a dozen loose entities. The
// configuration URL is the link back to this UI, and is left out when nobody has said what this
// server is called from outside its own container — a link to a guess is worse than no link.
func (b *Bridge) device() map[string]any {
	device := map[string]any{
		"identifiers":  []string{b.nodeID},
		"name":         "Google Photos backup",
		"manufacturer": "gpb",
		"model":        "Google Photos backup",
	}
	if b.externalURL != "" {
		device["configuration_url"] = b.externalURL
	}
	return device
}
