package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedPatch struct {
	ThreadID  string
	Archived  bool
	Locked    bool
	Auth      string
	UserAgent string
}

// fakeBotAPI stands in for PATCH /channels/{id}, the one call a webhook cannot
// make and the reason a bot token exists here at all.
type fakeBotAPI struct {
	server  *httptest.Server
	mutex   sync.Mutex
	patches []recordedPatch
	status  int
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()

	fake := &fakeBotAPI{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Archived bool `json:"archived"`
			Locked   bool `json:"locked"`
		}
		_ = json.Unmarshal(body, &payload)

		fake.mutex.Lock()
		fake.patches = append(fake.patches, recordedPatch{
			ThreadID:  strings.TrimPrefix(r.URL.Path, "/channels/"),
			Archived:  payload.Archived,
			Locked:    payload.Locked,
			Auth:      r.Header.Get("Authorization"),
			UserAgent: r.Header.Get("User-Agent"),
		})
		status := fake.status
		fake.mutex.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

func (f *fakeBotAPI) recorded() []recordedPatch {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]recordedPatch(nil), f.patches...)
}

func TestCloseThreadArchivesWithoutLocking(t *testing.T) {
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: bot.server.Client(), botAPIBase: bot.server.URL}

	if err := instance.closeThread(context.Background(), "s3cr3t", "t1"); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := bot.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d patches, want 1", len(got))
	}
	if got[0].Auth != "Bot s3cr3t" {
		t.Errorf("Authorization = %q, want the Bot scheme", got[0].Auth)
	}
	if !got[0].Archived {
		t.Error("the post should be archived")
	}
	// Without this Discord's WAF answers a Cloudflare 1010 -- which no local
	// test server would ever reproduce, so it is asserted here instead.
	if got[0].UserAgent != discordUserAgent {
		t.Errorf("User-Agent = %q, want %q", got[0].UserAgent, discordUserAgent)
	}
	// Locking would reject a postmortem posted after the resolution, because a
	// webhook has no permission to post through a lock. Archiving alone reopens
	// on the next message, which is what makes that late update land.
	if got[0].Locked {
		t.Error("the post must not be locked")
	}
}

// The close budget is two channel edits per ten minutes for a thread, so it must
// fire on the resolution only -- not on each of investigating, identified and
// monitoring.
func TestSyncThreadClosedOnlyClosesOnResolution(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	entry := incident(
		update("u1", "investigating", 0),
		update("u2", "identified", 10*time.Minute),
		update("u3", "monitoring", 20*time.Minute),
		update("u4", "resolved", 30*time.Minute),
	)

	posted, err := instance.deliver(context.Background(), fake.server.URL, "s3cr3t", testSettings(), []statusEntry{entry}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if posted != 4 {
		t.Fatalf("posted = %d, want 4", posted)
	}

	if got := bot.recorded(); len(got) != 1 {
		t.Fatalf("got %d patches, want exactly 1 (the resolution)", len(got))
	}
	if !state.Entries["inc-1"].ThreadClosed {
		t.Error("the post should be recorded as closed")
	}
}

// A thread opening on its own resolution -- everything before it fell outside
// MAX_UPDATE_AGE -- still has to be closed.
func TestSyncThreadClosedClosesAThreadThatOpensResolved(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	if _, err := instance.deliver(context.Background(), fake.server.URL, "s3cr3t", testSettings(), []statusEntry{incident(update("u1", "resolved", 0))}, state, at(time.Hour)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got := bot.recorded(); len(got) != 1 || !got[0].Archived {
		t.Fatalf("got %+v, want the post closed", got)
	}
}

// Statuspage appends postmortems after the resolution. Posting reopens the
// archived post by itself, so the state has to notice, or the post would be
// left open -- the close only happens on a terminal update.
func TestSyncThreadClosedNotesAPostmortemReopeningThePost(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	entry := incident(update("u1", "investigating", 0), update("u2", "resolved", 10*time.Minute))
	if _, err := instance.deliver(context.Background(), fake.server.URL, "s3cr3t", testSettings(), []statusEntry{entry}, state, at(time.Hour)); err != nil {
		t.Fatalf("first deliver: %v", err)
	}
	if !state.Entries["inc-1"].ThreadClosed {
		t.Fatal("the post should be closed after the resolution")
	}

	// Statuspage marks a postmortem with its own status, which is terminal too;
	// an ordinary follow-up is not, and that is the one that leaves it open.
	entry.IncidentUpdates = append(entry.IncidentUpdates, update("u3", "monitoring", 20*time.Minute))
	if _, err := instance.deliver(context.Background(), fake.server.URL, "s3cr3t", testSettings(), []statusEntry{entry}, state, at(time.Hour)); err != nil {
		t.Fatalf("second deliver: %v", err)
	}

	if state.Entries["inc-1"].ThreadClosed {
		t.Error("posting reopened the post, so it must no longer be recorded as closed")
	}
}

// Without a token nothing is closed and nothing fails: the run behaves exactly
// as it did before the bot existed.
func TestSyncThreadClosedIsANoOpWithoutAToken(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	entry := incident(update("u1", "investigating", 0), update("u2", "resolved", 10*time.Minute))

	if _, err := instance.deliver(context.Background(), fake.server.URL, "", testSettings(), []statusEntry{entry}, state, at(time.Hour)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got := bot.recorded(); len(got) != 0 {
		t.Fatalf("got %d patches without a token, want none", len(got))
	}
	if state.Entries["inc-1"].ThreadClosed {
		t.Error("nothing was closed, so nothing should be recorded as closed")
	}
}

// A close that fails is cosmetic: the update was already posted, so the run must
// succeed, and the post must stay unrecorded so the next terminal update retries.
func TestSyncThreadClosedSurvivesAFailedClose(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	bot.status = http.StatusForbidden
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	entry := incident(update("u1", "investigating", 0), update("u2", "resolved", 10*time.Minute))

	posted, err := instance.deliver(context.Background(), fake.server.URL, "s3cr3t", testSettings(), []statusEntry{entry}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("a failed close must not fail the run: %v", err)
	}
	if posted != 2 {
		t.Fatalf("posted = %d, want both updates delivered", posted)
	}
	if state.Entries["inc-1"].ThreadClosed {
		t.Error("a failed close must not be recorded, or it never retries")
	}
}

// The title carries no status marker any more: open versus closed is the status.
func TestThreadNameCarriesNoStatusMarker(t *testing.T) {
	for _, status := range []string{"investigating", "resolved"} {
		entry := incident(update("u1", status, 0))
		payload := buildPayload(entry, entry.IncidentUpdates[0], true, testSettings())

		if !strings.HasPrefix(payload.ThreadName, "Claude · ") {
			t.Errorf("%s: thread name = %q, want it to start with the page label", status, payload.ThreadName)
		}
	}
}
