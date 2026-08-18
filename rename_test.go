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

type recordedRename struct {
	ThreadID string
	Name     string
	Auth     string
}

// fakeBotAPI stands in for PATCH /channels/{id}, the one call a webhook cannot
// make and the reason a bot token exists here at all.
type fakeBotAPI struct {
	server  *httptest.Server
	mutex   sync.Mutex
	renames []recordedRename
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
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &payload)

		fake.mutex.Lock()
		fake.renames = append(fake.renames, recordedRename{
			ThreadID: strings.TrimPrefix(r.URL.Path, "/channels/"),
			Name:     payload.Name,
			Auth:     r.Header.Get("Authorization"),
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

func (f *fakeBotAPI) recorded() []recordedRename {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]recordedRename(nil), f.renames...)
}

func TestRenameThreadSendsTheBotIdentity(t *testing.T) {
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: bot.server.Client(), botAPIBase: bot.server.URL}

	if err := instance.renameThread(context.Background(), "s3cr3t", "t1", "🟢 Claude · resolved"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got := bot.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d renames, want 1", len(got))
	}
	if got[0].Auth != "Bot s3cr3t" {
		t.Errorf("Authorization = %q, want the Bot scheme", got[0].Auth)
	}
	if got[0].Name != "🟢 Claude · resolved" {
		t.Errorf("name = %q", got[0].Name)
	}
}

// The rename budget is two per ten minutes for a thread, so it must only fire
// when the phase actually flips -- not on each of investigating, identified and
// monitoring.
func TestSyncThreadTitleOnlyRenamesWhenThePhaseChanges(t *testing.T) {
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

	renames := bot.recorded()
	if len(renames) != 1 {
		t.Fatalf("got %d renames, want exactly 1 (firing -> resolved)", len(renames))
	}
	if !strings.HasPrefix(renames[0].Name, "🟢") {
		t.Errorf("renamed to %q, want the resolved marker", renames[0].Name)
	}
	if state.Entries["inc-1"].TitlePhase != phaseResolved {
		t.Errorf("stored phase = %q, want %q", state.Entries["inc-1"].TitlePhase, phaseResolved)
	}
}

// Without a token the run must behave exactly as before, and must still record
// the phase so that adding a token later does not rewrite every old title.
func TestSyncThreadTitleIsANoOpWithoutAToken(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	entry := incident(update("u1", "investigating", 0), update("u2", "resolved", 10*time.Minute))

	if _, err := instance.deliver(context.Background(), fake.server.URL, "", testSettings(), []statusEntry{entry}, state, at(time.Hour)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got := bot.recorded(); len(got) != 0 {
		t.Fatalf("got %d renames without a token, want none", len(got))
	}
	if state.Entries["inc-1"].TitlePhase != phaseResolved {
		t.Errorf("stored phase = %q, want the phase recorded anyway", state.Entries["inc-1"].TitlePhase)
	}
}

// A rename that fails is cosmetic damage only: the update was already posted,
// so the run must not report an error, and the phase must stay unrecorded so
// the next run tries again.
func TestSyncThreadTitleSurvivesAFailedRename(t *testing.T) {
	fake := newFakeDiscord(t)
	bot := newFakeBotAPI(t)
	bot.status = http.StatusForbidden
	instance := &app{httpClient: fake.server.Client(), botAPIBase: bot.server.URL}
	state := newState()

	// Two updates: the first opens the thread, the second is the one whose phase
	// flip needs a rename.
	entry := incident(update("u1", "investigating", 0), update("u2", "resolved", 10*time.Minute))

	posted, err := instance.deliver(context.Background(), fake.server.URL, "s3cr3t", testSettings(), []statusEntry{entry}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("a failed rename must not fail the run: %v", err)
	}
	if posted != 2 {
		t.Fatalf("posted = %d, want both updates still delivered", posted)
	}
	if len(bot.recorded()) != 1 {
		t.Fatalf("got %d rename attempts, want 1", len(bot.recorded()))
	}
	if state.Entries["inc-1"].TitlePhase == phaseResolved {
		t.Error("a failed rename must not record the phase, or it never retries")
	}
}

// A thread whose earlier updates fell outside MAX_UPDATE_AGE opens on its
// resolution, and should open green rather than red-then-immediately-renamed.
func TestThreadOpensGreenWhenItStartsResolved(t *testing.T) {
	entry := incident(update("u1", "resolved", 0))

	payload := buildPayload(entry, entry.IncidentUpdates[0], true, testSettings())

	if !strings.HasPrefix(payload.ThreadName, "🟢") {
		t.Errorf("thread name = %q, want it to open with the resolved marker", payload.ThreadName)
	}
}
