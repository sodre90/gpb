package gphotos

import (
	"errors"
	"fmt"
	"strings"
)

// ErrProtocolDrift means Google changed a response shape underneath us. Decoders return it
// instead of limping on with partial data: a half-decoded album listing would quietly sync
// the wrong set, whereas a loud failure gets a fixture re-recorded and the decoder fixed.
var ErrProtocolDrift = errors.New("google photos response did not match the expected shape")

// tree is a positional reader over a decoded batchexecute payload. Google's arrays are
// heterogeneous, sparse and long, so every step records the path it walked; a shape change
// then surfaces as a drift error naming the exact position instead of a nil-map panic.
type tree struct {
	value any
	path  string
}

func rootOf(value any, label string) tree {
	return tree{value: value, path: label}
}

func (t tree) at(index int) tree {
	child := tree{path: fmt.Sprintf("%s[%d]", t.path, index)}
	if list, ok := t.value.([]any); ok && index >= 0 && index < len(list) {
		child.value = list[index]
	}
	return child
}

func (t tree) key(name string) tree {
	child := tree{path: fmt.Sprintf("%s[%q]", t.path, name)}
	if object, ok := t.value.(map[string]any); ok {
		child.value = object[name]
	}
	return child
}

// last reads the final element of an array. The shared-album listing puts its metadata record
// there, and the entries around it vary in length by album kind, so counting from the end is
// the only position that holds for all of them.
func (t tree) last() tree {
	list, ok := t.value.([]any)
	if !ok || len(list) == 0 {
		return tree{path: t.path + "[last]"}
	}
	return t.at(len(list) - 1)
}

// only reads the sole value of a single-entry object. Google keys that record by a field
// number rather than a name, so reading it by key would hard-code a digit string that carries
// no meaning here and would drift the day the field is renumbered.
func (t tree) only() tree {
	child := tree{path: t.path + "{}"}
	object, ok := t.value.(map[string]any)
	if !ok || len(object) != 1 {
		return child
	}
	for _, value := range object {
		child.value = value
	}
	return child
}

func (t tree) present() bool { return t.value != nil }

func (t tree) text() (string, bool) {
	value, ok := t.value.(string)
	return value, ok
}

func (t tree) number() (float64, bool) {
	value, ok := t.value.(float64)
	return value, ok
}

func (t tree) list() ([]any, bool) {
	value, ok := t.value.([]any)
	return value, ok
}

// cursor reads a page token, where an absent one is the ordinary end of a walk and an
// unreadable one is drift. The distinction matters more here than anywhere else in this
// package: a caller reads the empty token as "the listing is complete" and reconciles the
// album against it, so a cursor that is present but not a string has to raise rather than
// come back empty and be mistaken for the end.
func (t tree) cursor() (string, error) {
	if !t.present() {
		return "", nil
	}
	token, ok := t.text()
	if !ok {
		return "", t.driftf("a page cursor")
	}
	return token, nil
}

func (t tree) driftf(want string) error {
	return fmt.Errorf("%w: wanted %s at %s, found %s", ErrProtocolDrift, want, t.path, t.describe())
}

// describe names the shape found without echoing its contents: payloads carry media keys,
// signed URLs and album titles, none of which belong in an error string that reaches a log.
func (t tree) describe() string {
	switch value := t.value.(type) {
	case nil:
		return "nothing"
	case string:
		return fmt.Sprintf("a %d-character string", len(value))
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return fmt.Sprintf("a %d-element array", len(value))
	case map[string]any:
		return fmt.Sprintf("an object with %d keys", len(value))
	default:
		return "an unknown type"
	}
}

// sampleShape renders the skeleton of a payload — arity and types, never values — so a drift
// report can travel in a log line or a notification without leaking the user's library.
func sampleShape(value any, depth int) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return "str"
	case float64:
		return "num"
	case bool:
		return "bool"
	case []any:
		if depth == 0 {
			return fmt.Sprintf("[…%d]", len(typed))
		}
		parts := make([]string, 0, len(typed))
		for _, element := range typed {
			parts = append(parts, sampleShape(element, depth-1))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		return fmt.Sprintf("{…%d}", len(typed))
	default:
		return "?"
	}
}
