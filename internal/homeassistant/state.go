// Package homeassistant puts what this backup is doing on an MQTT broker, in the shape Home
// Assistant discovers: the entities announce themselves, so nobody has to write YAML to see a
// backup that takes days on a dashboard beside the lights.
package homeassistant

// State is the whole of what gets published, in one retained message. Every field is a scalar so
// that two states can be compared outright — the bridge publishes only when something has
// actually moved, and a run downloading at two files a second would otherwise fill the broker
// with messages saying the same thing.
type State struct {
	Activity    string  `json:"activity"`
	Running     bool    `json:"running"`
	BackedUp    int     `json:"backed_up"`
	Remaining   int     `json:"remaining"`
	Progress    int     `json:"progress"`
	Downloading string  `json:"downloading"`
	Album       string  `json:"album"`
	Failed      int     `json:"failed"`
	LastRunAt   string  `json:"last_run_at"`
	LastOutcome string  `json:"last_outcome"`
	FreeGB      float64 `json:"free_gb"`
	// SessionProblem is a truth rather than a word because the entity reading it is a problem
	// sensor, and Home Assistant reads any non-empty word as a problem — "ok" included.
	SessionProblem bool `json:"session_problem"`
	// Library is the whole-library setting in the words the selector offers, because a select
	// entity's state has to be one of its own options or Home Assistant shows it as unknown.
	Library string `json:"library"`
}

// Source is where the bridge reads from, once a second. It is an interface so that this package
// knows nothing about the store, the runner or the browser session — it turns one struct into
// MQTT and does not care who filled it in.
type Source interface {
	Snapshot() State
}
