package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStateRecordsUpdatesWithoutDuplicating(t *testing.T) {
	state := newState()
	entry := incident(update("u1", "investigating", 0))

	state.record(entry, "u1", at(0))
	state.record(entry, "u1", at(0))

	if got := state.Entries["inc-1"].PostedUpdates; len(got) != 1 {
		t.Errorf("posted updates = %v, want one entry", got)
	}
	if !state.hasPosted("inc-1", "u1") {
		t.Error("hasPosted should report the recorded update")
	}
	if state.hasPosted("inc-1", "u2") || state.hasPosted("inc-2", "u1") {
		t.Error("hasPosted should not report updates that were never recorded")
	}
}

// LastPostedAt drives pruning, so an update that arrives out of order must not
// pull it backwards.
func TestStateKeepsTheLatestPostedTime(t *testing.T) {
	state := newState()
	entry := incident(update("u1", "investigating", 0))

	state.record(entry, "u2", at(time.Hour))
	state.record(entry, "u1", at(0))

	if got := state.Entries["inc-1"].LastPostedAt; !got.Equal(at(time.Hour)) {
		t.Errorf("last posted at = %s, want the later time %s", got, at(time.Hour))
	}
}

func TestStatePrunesOnlyEntriesPastRetention(t *testing.T) {
	state := newState()
	state.Entries["old"] = entryState{LastPostedAt: at(-40 * 24 * time.Hour)}
	state.Entries["recent"] = entryState{LastPostedAt: at(-2 * 24 * time.Hour)}

	state.prune(at(0), defaultStateRetention)

	if _, kept := state.Entries["old"]; kept {
		t.Error("the entry past retention should have been pruned")
	}
	if _, kept := state.Entries["recent"]; !kept {
		t.Error("the recent entry should have been kept")
	}
}

func TestStateRoundTripsThroughJSON(t *testing.T) {
	state := newState()
	state.setThreadID("inc-1", "thread-1")
	state.record(incident(update("u1", "investigating", 0)), "u1", at(0))

	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	decoded := newState()
	if err := json.Unmarshal(encoded, decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Entries["inc-1"].ThreadID != "thread-1" {
		t.Errorf("thread id did not survive the round trip: %+v", decoded.Entries["inc-1"])
	}
	if !decoded.hasPosted("inc-1", "u1") {
		t.Error("posted updates did not survive the round trip")
	}
}

func TestParseWebhookURLAcceptsTheShapesSSMMightHold(t *testing.T) {
	const want = "https://discord.com/api/webhooks/1/token"

	for name, raw := range map[string]string{
		"plain":       want,
		"padded":      "  " + want + "  ",
		"json string": `"` + want + `"`,
		"json object": `{"url": "` + want + `"}`,
	} {
		got, err := parseWebhookURL(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}

	for name, raw := range map[string]string{
		"empty":     "   ",
		"not https": "http://discord.com/api/webhooks/1/token",
		"not a URL": "webhooks/1/token",
	} {
		if _, err := parseWebhookURL(raw); err == nil {
			t.Errorf("%s should have been rejected", name)
		}
	}
}

func TestDurationEnvRejectsNonPositiveValues(t *testing.T) {
	t.Setenv("MAX_UPDATE_AGE", "0s")
	if _, err := durationEnv("MAX_UPDATE_AGE", defaultMaxUpdateAge); err == nil {
		t.Error("a zero cutoff would suppress every update, so it must be rejected")
	}

	t.Setenv("MAX_UPDATE_AGE", "")
	got, err := durationEnv("MAX_UPDATE_AGE", defaultMaxUpdateAge)
	if err != nil {
		t.Fatalf("durationEnv: %v", err)
	}
	if got != defaultMaxUpdateAge {
		t.Errorf("got %s, want the default %s", got, defaultMaxUpdateAge)
	}
}

func TestMigrateStateMarksVersionOneThreadsAsMentioned(t *testing.T) {
	state := &notifierState{Version: 1, Entries: map[string]entryState{
		"posted":   {ThreadID: "thread-1"},
		"absorbed": {PostedUpdates: []string{"u1"}},
	}}

	migrateState(state)

	if state.Version != stateVersion {
		t.Errorf("Version = %d, want %d", state.Version, stateVersion)
	}
	if !state.Entries["posted"].Mentioned {
		t.Error("a version 1 entry with a thread opened with a mention and should be marked so")
	}
	if state.Entries["absorbed"].Mentioned {
		t.Error("an entry that never got a thread never mentioned anyone")
	}
}

func TestMigrateStateLeavesCurrentEntriesAlone(t *testing.T) {
	state := &notifierState{Version: stateVersion, Entries: map[string]entryState{
		"quiet": {ThreadID: "thread-1"},
	}}

	migrateState(state)

	if state.Entries["quiet"].Mentioned {
		t.Error("a current-version entry without the flag opened quietly and must stay unmentioned")
	}
}
