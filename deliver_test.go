package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedPost struct {
	ThreadID string
	Payload  webhookPayload
}

// fakeDiscord stands in for the webhook endpoint. respond lets a test fail a
// specific call; returning 0 means "succeed and mint a thread id".
type fakeDiscord struct {
	server  *httptest.Server
	mutex   sync.Mutex
	posts   []recordedPost
	respond func(call int, post recordedPost) (int, string)
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()

	fake := &fakeDiscord{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		var payload webhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if r.URL.Query().Get("wait") != "true" {
			t.Errorf("wait=true is required to learn the thread id, got %q", r.URL.RawQuery)
		}

		post := recordedPost{ThreadID: r.URL.Query().Get("thread_id"), Payload: payload}

		fake.mutex.Lock()
		call := len(fake.posts)
		fake.posts = append(fake.posts, post)
		responder := fake.respond
		fake.mutex.Unlock()

		if responder != nil {
			if status, responseBody := responder(call, post); status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, responseBody)
				return
			}
		}

		threadID := post.ThreadID
		if threadID == "" {
			threadID = fmt.Sprintf("thread-%d", call+1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webhookMessage{ID: fmt.Sprintf("message-%d", call+1), ChannelID: threadID})
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

func (f *fakeDiscord) recorded() []recordedPost {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]recordedPost(nil), f.posts...)
}

func testApp(fake *fakeDiscord) *app {
	return &app{httpClient: fake.server.Client()}
}

func testSettings() settings {
	return settings{
		baseURL:        "https://status.claude.com/api/v2",
		pageLabel:      "Claude",
		pageHost:       "status.claude.com",
		maxUpdateAge:   24 * time.Hour,
		stateRetention: defaultStateRetention,
	}
}

func at(offset time.Duration) time.Time {
	return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC).Add(offset)
}

func incident(updates ...statusUpdate) statusEntry {
	return statusEntry{
		ID:              "inc-1",
		Name:            "Service disruption on Claude services",
		Status:          updates[len(updates)-1].Status,
		Impact:          "critical",
		Shortlink:       "https://stspg.io/example",
		CreatedAt:       at(0),
		UpdatedAt:       at(time.Hour),
		IncidentUpdates: updates,
		Kind:            kindIncident,
	}
}

func update(id, status string, offset time.Duration) statusUpdate {
	return statusUpdate{
		ID:        id,
		Status:    status,
		Body:      "Body of " + id,
		CreatedAt: at(offset),
		DisplayAt: at(offset),
	}
}

func TestDeliverOpensThreadThenFollowsUpInIt(t *testing.T) {
	fake := newFakeDiscord(t)
	state := newState()

	// Deliberately newest-first, the order Statuspage actually serves them in.
	entry := incident(
		update("u2", "resolved", 30*time.Minute),
		update("u1", "investigating", 0),
	)

	posted, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{entry}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if posted != 2 {
		t.Fatalf("posted = %d, want 2", posted)
	}

	posts := fake.recorded()
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(posts))
	}

	// The oldest update opens the forum post; it is the only one carrying
	// thread_name, and it must not carry thread_id.
	if posts[0].Payload.ThreadName == "" {
		t.Error("first post must open a thread via thread_name")
	}
	if posts[0].ThreadID != "" {
		t.Errorf("first post must not target a thread, got thread_id=%q", posts[0].ThreadID)
	}
	if !strings.Contains(posts[0].Payload.Content, "Investigating") {
		t.Errorf("first post content = %q, want the oldest update", posts[0].Payload.Content)
	}

	// The follow-up goes into the thread the first post created.
	if posts[1].Payload.ThreadName != "" {
		t.Error("follow-up must not open a second thread")
	}
	if posts[1].ThreadID != "thread-1" {
		t.Errorf("follow-up thread_id = %q, want thread-1", posts[1].ThreadID)
	}
	if !strings.Contains(posts[1].Payload.Content, "Resolved") {
		t.Errorf("follow-up content = %q, want the resolved update", posts[1].Payload.Content)
	}

	if got := state.Entries["inc-1"].ThreadID; got != "thread-1" {
		t.Errorf("stored thread id = %q, want thread-1", got)
	}
	if !state.hasPosted("inc-1", "u1") || !state.hasPosted("inc-1", "u2") {
		t.Errorf("both updates should be recorded, got %v", state.Entries["inc-1"].PostedUpdates)
	}
}

func TestDeliverSkipsUpdatesAlreadyPosted(t *testing.T) {
	fake := newFakeDiscord(t)
	state := newState()
	entry := incident(update("u1", "investigating", 0))

	instance := testApp(fake)
	if _, err := instance.deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{entry}, state, at(time.Hour)); err != nil {
		t.Fatalf("first deliver: %v", err)
	}

	posted, err := instance.deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{entry}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("second deliver: %v", err)
	}
	if posted != 0 {
		t.Fatalf("second run posted %d updates, want 0", posted)
	}
	if len(fake.recorded()) != 1 {
		t.Fatalf("got %d posts across both runs, want 1", len(fake.recorded()))
	}
}

// The cold-start guard: with empty state, a page full of incidents that were
// resolved days ago must be absorbed silently rather than replayed.
func TestDeliverSuppressesUpdatesOlderThanTheCutoff(t *testing.T) {
	fake := newFakeDiscord(t)
	state := newState()
	entry := incident(
		update("u1", "investigating", -72*time.Hour),
		update("u2", "resolved", -71*time.Hour),
	)

	posted, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{entry}, state, at(0))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if posted != 0 {
		t.Fatalf("posted = %d, want 0", posted)
	}
	if len(fake.recorded()) != 0 {
		t.Fatalf("got %d posts, want none", len(fake.recorded()))
	}

	// Recorded anyway, so a later run cannot decide to post them after all.
	if !state.hasPosted("inc-1", "u1") || !state.hasPosted("inc-1", "u2") {
		t.Errorf("suppressed updates must still be recorded, got %v", state.Entries["inc-1"].PostedUpdates)
	}
}

// An incident that opened before the cutoff but is still receiving updates has
// to start notifying, from the first update inside the window.
func TestDeliverOpensThreadFromTheFirstUpdateInsideTheCutoff(t *testing.T) {
	fake := newFakeDiscord(t)
	state := newState()
	entry := incident(
		update("u1", "investigating", -48*time.Hour),
		update("u2", "monitoring", -time.Hour),
	)

	posted, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{entry}, state, at(0))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}

	posts := fake.recorded()
	if posts[0].Payload.ThreadName == "" {
		t.Error("the first posted update must open the thread even though it is not the incident's first update")
	}
	if !strings.Contains(posts[0].Payload.Content, "Monitoring") {
		t.Errorf("content = %q, want the monitoring update", posts[0].Payload.Content)
	}
}

func TestDeliverMentionsTheRoleOnOpenAndOnResolution(t *testing.T) {
	fake := newFakeDiscord(t)
	state := newState()
	current := testSettings()
	current.mentionRoleID = "987654321"

	entry := incident(
		update("u1", "investigating", 0),
		update("u2", "identified", 10*time.Minute),
		update("u3", "resolved", 20*time.Minute),
	)

	if _, err := testApp(fake).deliver(context.Background(), fake.server.URL, current, []statusEntry{entry}, state, at(time.Hour)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	posts := fake.recorded()
	if len(posts) != 3 {
		t.Fatalf("got %d posts, want 3", len(posts))
	}

	mentioned := []bool{true, false, true}
	for index, want := range mentioned {
		hasMention := strings.Contains(posts[index].Payload.Content, "<@&987654321>")
		if hasMention != want {
			t.Errorf("post %d mention = %v, want %v (content %q)", index, hasMention, want, posts[index].Payload.Content)
		}

		roles := posts[index].Payload.AllowedMentions.Roles
		if want && (len(roles) != 1 || roles[0] != "987654321") {
			t.Errorf("post %d allowed_mentions.roles = %v, want the role to be allowed", index, roles)
		}
		if !want && len(roles) != 0 {
			t.Errorf("post %d allowed_mentions.roles = %v, want empty", index, roles)
		}
	}

	// @everyone must never be reachable, whatever an incident body contains.
	for index, post := range posts {
		if len(post.Payload.AllowedMentions.Parse) != 0 {
			t.Errorf("post %d allowed_mentions.parse = %v, want empty", index, post.Payload.AllowedMentions.Parse)
		}
	}
}

func TestDeliverStopsAnEntryAtItsFirstFailureButContinuesWithTheNext(t *testing.T) {
	fake := newFakeDiscord(t)
	// 400 rather than 500, so the post fails outright instead of being retried.
	fake.respond = func(call int, post recordedPost) (int, string) {
		if call == 0 {
			return http.StatusBadRequest, `{"message":"Invalid Form Body"}`
		}
		return 0, ""
	}

	first := incident(update("u1", "investigating", 0), update("u2", "resolved", 10*time.Minute))
	second := statusEntry{
		ID:              "inc-2",
		Name:            "Elevated errors",
		Impact:          "minor",
		CreatedAt:       at(time.Minute),
		IncidentUpdates: []statusUpdate{update("u3", "investigating", 20*time.Minute)},
		Kind:            kindIncident,
	}

	state := newState()
	posted, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{first, second}, state, at(time.Hour))
	if err == nil {
		t.Fatal("deliver should report the failed post")
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1 (only the second incident)", posted)
	}

	// Nothing about the failed incident may be recorded, so the next run retries
	// it from the top rather than skipping straight to its resolution.
	if _, recorded := state.Entries["inc-1"]; recorded {
		t.Errorf("failed entry must not be recorded, got %+v", state.Entries["inc-1"])
	}
	if !state.hasPosted("inc-2", "u3") {
		t.Error("the second incident should have been delivered and recorded")
	}
}

// The retry path: the first attempt is rejected with 500 twice, the third
// succeeds, and deliver reports no error.
func TestPostWebhookRetriesServerErrors(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.respond = func(call int, post recordedPost) (int, string) {
		if call < 2 {
			return http.StatusInternalServerError, "try again"
		}
		return 0, ""
	}

	state := newState()
	posted, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{incident(update("u1", "investigating", 0))}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}
	if len(fake.recorded()) != 3 {
		t.Fatalf("got %d attempts, want 3", len(fake.recorded()))
	}
}

// A thread deleted in Discord answers 404 forever; the entry has to recover by
// opening a replacement rather than wedging.
func TestDeliverReopensAThreadThatIsGone(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.respond = func(call int, post recordedPost) (int, string) {
		if post.ThreadID == "thread-1" {
			return http.StatusNotFound, `{"message":"Unknown Channel","code":10003}`
		}
		return 0, ""
	}

	state := newState()
	state.Entries["inc-1"] = entryState{ThreadID: "thread-1", Name: "Service disruption on Claude services"}

	posted, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{incident(update("u1", "monitoring", 0))}, state, at(time.Hour))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if posted != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}

	posts := fake.recorded()
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2 (the 404 then the replacement)", len(posts))
	}
	if posts[1].Payload.ThreadName == "" {
		t.Error("the replacement post must open a new thread")
	}
	if got := state.Entries["inc-1"].ThreadID; got == "thread-1" || got == "" {
		t.Errorf("stored thread id = %q, want the replacement thread", got)
	}
}

// A 404 is not retried as a rate limit or a server error would be: exactly one
// attempt per target.
func TestDeliverDoesNotRetryClientErrorsOtherThanMissingThreads(t *testing.T) {
	fake := newFakeDiscord(t)
	fake.respond = func(call int, post recordedPost) (int, string) {
		return http.StatusBadRequest, `{"message":"Invalid Form Body"}`
	}

	state := newState()
	if _, err := testApp(fake).deliver(context.Background(), fake.server.URL, testSettings(), []statusEntry{incident(update("u1", "investigating", 0))}, state, at(time.Hour)); err == nil {
		t.Fatal("deliver should report the rejection")
	}
	if len(fake.recorded()) != 1 {
		t.Fatalf("got %d attempts, want 1", len(fake.recorded()))
	}
}
