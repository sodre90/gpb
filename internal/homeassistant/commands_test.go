package homeassistant

import (
	"errors"
	"testing"
)

var errNoBroker = errors.New("no broker")

func TestPressingBackUpNowStartsARun(t *testing.T) {
	bridge, broker, _, commands := testBridge(t)
	bridge.announce()

	broker.deliver(t, "gpb/command/back_up_now", "PRESS")

	if len(commands.syncs) != 1 {
		t.Fatalf("the button started %d runs, want 1", len(commands.syncs))
	}
	if commands.syncs[0] != askedFromHomeAssistant {
		t.Errorf("the run was started because %q, which will not read well in the log",
			commands.syncs[0])
	}
	if len(commands.refreshes) != 0 {
		t.Errorf("pressing one button also did %d album refreshes", len(commands.refreshes))
	}
}

func TestPressingRefreshRereadsTheAlbumList(t *testing.T) {
	bridge, broker, _, commands := testBridge(t)
	bridge.announce()

	broker.deliver(t, "gpb/command/refresh_albums", "PRESS")

	if len(commands.refreshes) != 1 {
		t.Errorf("the button ran %d album refreshes, want 1", len(commands.refreshes))
	}
}

// The selector offers the words the web UI uses; the store has its own. A mapping that let the
// display words through would set the library to a mode that means nothing.
func TestTheLibrarySelectorSpeaksTheStoresWords(t *testing.T) {
	bridge, broker, _, commands := testBridge(t)
	bridge.announce()

	broker.deliver(t, "gpb/command/library", "Everything")
	broker.deliver(t, "gpb/command/library", "No")
	broker.deliver(t, "gpb/command/library", "all")

	if len(commands.library) != 2 || commands.library[0] != "all" || commands.library[1] != "none" {
		t.Errorf("the library was set to %v, want all then none and nothing for a word it does not offer",
			commands.library)
	}
}

// Two hundred albums would be two hundred entities, so album control is a topic an automation
// publishes to instead. The album id is part of the topic, which is the whole of the addressing.
func TestAnAlbumsModeCanBeSetByTopic(t *testing.T) {
	bridge, broker, _, commands := testBridge(t)
	bridge.announce()

	broker.deliver(t, "gpb/album/album-1/mode/set", "picked")

	if commands.albumModes["album-1"] != "picked" {
		t.Errorf("album-1 was set to %q, want picked", commands.albumModes["album-1"])
	}
}

// This is a topic on somebody's home network. Anything can publish to it, including things that
// are not commands at all, and none of that is a reason to take a backup down.
func TestNonsenseFromTheBrokerIsRefusedRatherThanObeyed(t *testing.T) {
	bridge, _, _, commands := testBridge(t)

	for _, nonsense := range []struct{ topic, payload string }{
		{"gpb/command/self_destruct", "PRESS"},
		{"gpb/command/", "PRESS"},
		{"gpb/album//mode/set", "all"},
		{"gpb/album/album-1/mode/set/extra", "all"},
		{"something/else/entirely", "PRESS"},
	} {
		if err := bridge.carryOut(nonsense.topic, nonsense.payload); err == nil {
			t.Errorf("%s %q was accepted", nonsense.topic, nonsense.payload)
		}
	}

	if len(commands.syncs)+len(commands.refreshes)+len(commands.library)+len(commands.albumModes) != 0 {
		t.Error("nonsense from the broker reached the runner")
	}
}

// A run refused because one is already going is the ordinary case, not a failure of the bridge:
// pressing the button twice must not take the connection down with it.
func TestACommandThatCannotBeCarriedOutIsSurvived(t *testing.T) {
	bridge, broker, _, commands := testBridge(t)
	commands.refuse = errors.New("a run is already in progress")
	bridge.announce()

	broker.deliver(t, "gpb/command/back_up_now", "PRESS")
	broker.deliver(t, "gpb/command/back_up_now", "PRESS")

	if len(commands.syncs) != 2 {
		t.Errorf("the bridge stopped asking after a refusal: %d runs asked for", len(commands.syncs))
	}
}
