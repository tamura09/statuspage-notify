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

	payload := buildPayload(entry, entry.IncidentUpdates[0], true, "")

	if payload.Username != webhookUsername {
		t.Errorf("username = %q, want %q", payload.Username, webhookUsername)
	}
	if !strings.HasPrefix(payload.ThreadName, "2026-08-16 ") {
		t.Errorf("thread name = %q, want it dated so repeat titles stay distinguishable", payload.ThreadName)
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

	payload := buildPayload(entry, entry.IncidentUpdates[0], false, "")

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

	payload := buildPayload(entry, maintenanceUpdate, true, "")

	if !strings.HasPrefix(payload.ThreadName, "2026-08-15 Maintenance: ") {
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

func TestBuildPayloadContentLeadsWithStatusAndName(t *testing.T) {
	entry := incident(update("u1", "monitoring", 0))

	// The content line is the push notification preview, so both facts have to
	// be in it rather than only in the embed.
	payload := buildPayload(entry, entry.IncidentUpdates[0], false, "")

	if !strings.Contains(payload.Content, "Monitoring") || !strings.Contains(payload.Content, entry.Name) {
		t.Errorf("content = %q, want the status and the incident name", payload.Content)
	}
}

func TestBuildPayloadStaysWithinDiscordLimits(t *testing.T) {
	entry := incident(update("u1", "investigating", 0))
	entry.Name = strings.Repeat("long incident name ", 40)
	entry.IncidentUpdates[0].Body = strings.Repeat("détail ", 2000)

	payload := buildPayload(entry, entry.IncidentUpdates[0], true, "123")

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
