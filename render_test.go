package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildPayloadRendersTheUpdate(t *testing.T) {
	entry := incident(update("u1", "investigating", 0))
	entry.IncidentUpdates[0].AffectedComponents = []affectedComponent{
		{Code: "b", Name: "Claude Code", OldStatus: "operational", NewStatus: "major_outage"},
		{Code: "a", Name: "claude.ai", OldStatus: "operational", NewStatus: "major_outage"},
		{Code: "c", Name: "Claude for Government", OldStatus: "operational", NewStatus: "operational"},
	}

	payload := buildPayload(entry, entry.IncidentUpdates[0], true, testSettings())

	// The username is what tells two status pages apart when they share a
	// channel, so it has to carry the page label.
	if payload.Username != "Claude Status" {
		t.Errorf("username = %q, want %q", payload.Username, "Claude Status")
	}
	if !strings.HasPrefix(payload.ThreadName, "Claude · 2026-08-16 ") {
		t.Errorf("thread name = %q, want it labelled and dated so entries from either page stay distinguishable", payload.ThreadName)
	}
	if !strings.Contains(payload.ThreadName, entry.Name) {
		t.Errorf("thread name = %q, want it to carry the incident name", payload.ThreadName)
	}

	if len(payload.Embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(payload.Embeds))
	}
	rendered := payload.Embeds[0]
	if rendered.Title != "Investigating" {
		t.Errorf("title = %q, want Investigating", rendered.Title)
	}
	if rendered.URL != entry.Shortlink {
		t.Errorf("url = %q, want the incident shortlink", rendered.URL)
	}
	if rendered.Color != colorCritical {
		t.Errorf("color = %#x, want the critical colour %#x", rendered.Color, colorCritical)
	}
	if rendered.Timestamp != "2026-08-16T12:00:00Z" {
		t.Errorf("timestamp = %q, want the update's own time", rendered.Timestamp)
	}

	components := fieldValue(t, rendered, "Affected components")
	if strings.Contains(components, "Claude for Government") {
		t.Errorf("components = %q, want the unchanged component left out", components)
	}
	if !strings.Contains(components, "claude.ai: operational → major outage") {
		t.Errorf("components = %q, want the transition spelled out", components)
	}

	if impact := fieldValue(t, rendered, "Impact"); impact != "Critical" {
		t.Errorf("impact = %q, want Critical", impact)
	}
}

func TestBuildPayloadColoursTerminalUpdatesGreenWhateverTheImpact(t *testing.T) {
	entry := incident(update("u1", "resolved", 0))

	payload := buildPayload(entry, entry.IncidentUpdates[0], false, testSettings())

	if got := payload.Embeds[0].Color; got != colorResolved {
		t.Errorf("color = %#x, want the resolved colour %#x", got, colorResolved)
	}
}

func TestBuildPayloadRendersMaintenanceWindows(t *testing.T) {
	start := at(0)
	end := at(2 * time.Hour)
	entry := statusEntry{
		ID:             "mnt-1",
		Name:           "Scheduled database maintenance",
		Impact:         "maintenance",
		CreatedAt:      at(-24 * time.Hour),
		ScheduledFor:   &start,
		ScheduledUntil: &end,
		Kind:           kindMaintenance,
	}
	maintenanceUpdate := update("u1", "scheduled", -24*time.Hour)

	payload := buildPayload(entry, maintenanceUpdate, true, testSettings())

	if !strings.HasPrefix(payload.ThreadName, "Claude · 2026-08-15 Maintenance: ") {
		t.Errorf("thread name = %q, want it marked as maintenance", payload.ThreadName)
	}
	if payload.Embeds[0].Color != colorMaintenance {
		t.Errorf("color = %#x, want the maintenance colour %#x", payload.Embeds[0].Color, colorMaintenance)
	}
	window := fieldValue(t, payload.Embeds[0], "Window")
	if !strings.Contains(window, "2026-08-16 12:00 UTC") || !strings.Contains(window, "2026-08-16 14:00 UTC") {
		t.Errorf("window = %q, want both ends of the scheduled window", window)
	}
}

// Maintenance is scheduled ahead and runs to a plan, so it never mentions the
// role -- not when its thread opens and not when it completes. An incident in
// the same channel does, and that contrast is the whole point: a ping has to
// mean something is wrong.
func TestBuildPayloadNeverMentionsForMaintenance(t *testing.T) {
	start := at(0)
	entry := statusEntry{
		ID:           "mnt-1",
		Name:         "Scheduled database maintenance",
		Impact:       "maintenance",
		CreatedAt:    at(-24 * time.Hour),
		ScheduledFor: &start,
		Kind:         kindMaintenance,
	}

	mentioning := testSettings()
	mentioning.mentionRoleID = "123"

	for _, status := range []string{"scheduled", "completed"} {
		payload := buildPayload(entry, update("u1", status, 0), status == "scheduled", mentioning)

		if strings.Contains(payload.Content, "<@&123>") {
			t.Errorf("%s content = %q, want no role mention", status, payload.Content)
		}
		if len(payload.AllowedMentions.Roles) != 0 {
			t.Errorf("%s allowed_mentions.roles = %v, want empty", status, payload.AllowedMentions.Roles)
		}
	}

	incidentEntry := incident(update("u1", "resolved", 0))
	resolved := buildPayload(incidentEntry, incidentEntry.IncidentUpdates[0], false, mentioning)
	if !strings.Contains(resolved.Content, "<@&123>") {
		t.Errorf("resolved incident content = %q, want the role mention", resolved.Content)
	}
}

func TestBuildPayloadContentLeadsWithStatusAndName(t *testing.T) {
	entry := incident(update("u1", "monitoring", 0))

	// The content line is the push notification preview, so both facts have to
	// be in it rather than only in the embed.
	payload := buildPayload(entry, entry.IncidentUpdates[0], false, testSettings())

	if !strings.Contains(payload.Content, "Monitoring") || !strings.Contains(payload.Content, entry.Name) {
		t.Errorf("content = %q, want the status and the incident name", payload.Content)
	}
}

func TestBuildPayloadStaysWithinDiscordLimits(t *testing.T) {
	entry := incident(update("u1", "investigating", 0))
	entry.Name = strings.Repeat("long incident name ", 40)
	entry.IncidentUpdates[0].Body = strings.Repeat("détail ", 2000)

	mentioning := testSettings()
	mentioning.mentionRoleID = "123"
	payload := buildPayload(entry, entry.IncidentUpdates[0], true, mentioning)

	if runes := len([]rune(payload.ThreadName)); runes > discordThreadNameLimit {
		t.Errorf("thread name is %d runes, over the %d limit", runes, discordThreadNameLimit)
	}
	if runes := len([]rune(payload.Content)); runes > discordContentLimit {
		t.Errorf("content is %d runes, over the %d limit", runes, discordContentLimit)
	}
	if runes := len([]rune(payload.Embeds[0].Description)); runes > discordEmbedDescriptionLimit {
		t.Errorf("description is %d runes, over the %d limit", runes, discordEmbedDescriptionLimit)
	}
}

// Two status pages posting into one channel have to be distinguishable at a
// glance, which is the whole job of the page label.
func TestBuildPayloadLabelsThePageItCameFrom(t *testing.T) {
	github := settings{pageLabel: "GitHub", pageHost: "githubstatus.com"}
	entry := incident(update("u1", "investigating", 0))
	entry.Name = "Incident with Actions"

	payload := buildPayload(entry, entry.IncidentUpdates[0], true, github)

	if payload.Username != "GitHub Status" {
		t.Errorf("username = %q, want GitHub Status", payload.Username)
	}
	if !strings.Contains(payload.ThreadName, "GitHub · ") {
		t.Errorf("thread name = %q, want the page label in front", payload.ThreadName)
	}
	if payload.Embeds[0].Footer.Text != "githubstatus.com" {
		t.Errorf("footer = %q, want the page host", payload.Embeds[0].Footer.Text)
	}
}

// An unset label must not break a deployment: it degrades to something neutral
// rather than rendering an empty username or a thread starting with a
// separator.
func TestBuildPayloadWithoutALabel(t *testing.T) {
	payload := buildPayload(incident(update("u1", "investigating", 0)), update("u1", "investigating", 0), true, settings{pageHost: "example.com"})

	if payload.Username != "Status" {
		t.Errorf("username = %q, want Status", payload.Username)
	}
	if strings.HasPrefix(payload.ThreadName, "·") || strings.HasPrefix(payload.ThreadName, " ") {
		t.Errorf("thread name = %q, want no dangling separator", payload.ThreadName)
	}
}

func TestPageHostIsWhatAReaderWouldVisit(t *testing.T) {
	for baseURL, want := range map[string]string{
		"https://status.claude.com/api/v2":    "status.claude.com",
		"https://www.githubstatus.com/api/v2": "githubstatus.com",
		"not a url":                           "not a url",
	} {
		if got := pageHost(baseURL); got != want {
			t.Errorf("pageHost(%q) = %q, want %q", baseURL, got, want)
		}
	}
}

func TestNormalizeBodyFoldsStatuspageLineEndings(t *testing.T) {
	got := normalizeBody("  first line\r\n\r\n\r\nsecond line\r\n  ")

	if got != "first line\n\nsecond line" {
		t.Errorf("normalizeBody = %q", got)
	}
	if normalizeBody("   ") != "(no details provided)" {
		t.Errorf("an empty body should still render something")
	}
}

func TestStatusLabelCoversBothLifecycles(t *testing.T) {
	for status, want := range map[string]string{
		"investigating": "Investigating",
		"resolved":      "Resolved",
		"in_progress":   "In progress",
		"completed":     "Completed",
		"brand_new":     "Brand_new",
	} {
		if got := statusLabel(status); got != want {
			t.Errorf("statusLabel(%q) = %q, want %q", status, got, want)
		}
	}
}

func fieldValue(t *testing.T, rendered embed, name string) string {
	t.Helper()
	for _, field := range rendered.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	t.Fatalf("embed has no %q field, got %+v", name, rendered.Fields)
	return ""
}
