package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// statusIQIncident renders one incident the way StatusIQ's history list does.
// The markup is trimmed from us.zohostatus.com, with the day cards and the
// attributes the parser does not read left out.
func statusIQIncident(id, severity, name, start, end, components string, rows ...string) string {
	return `<div class="py-2 mb-2">
  <a href="#" onclick="event.preventDefault();openNew('` + id + `')" onkeydown="if(event.key==='Enter'){event.preventDefault();openNew('` + id + `')}" class="incident-history-title txt-bold pointer text-decoration-none ` + severity + `" target="_blank" rel="noopener">` + name + ` <span class="mx-1 text-tertiary fs-sm icon-new-window tippy translate" data-content="incident.new.window"></span></a>
  <div>
    <p class="text-secondary mb-1" data-i18n="incident.duration"></p>
    <span class="mt-0 mb-3 incident-range" data-range="` + start + `','` + end + `"></span>
  </div>
  <div>
    <p class="text-secondary mb-1" data-i18n="incident.affected.components"></p>
    <p class="mt-0 mb-3"> ` + components + ` </p>
  </div>
  <div class="incident-update-container">` + strings.Join(rows, "") + `</div>
</div>`
}

func statusIQRow(stateKey, body, at string) string {
	return `<div class="update-row d-flex spList_200 " layout="row">
  <div class="update-icon-container"><span class="update-icon icon-resolved"></span></div>
  <div class="update-content-container flex-fill">
    <span class="txt-bold" data-i18n="` + stateKey + `"> </span>
    <div class="formatted-content wb-break-word">` + body + `</div>
    <div class="fs-sub-base update-footer"><span class="translate" data-args="global.posted.on','$0" data-date="` + at + `"> </span></div>
  </div>
</div>`
}

func statusIQPage(banner string, days ...string) string {
	cards := ""
	for _, day := range days {
		cards += `<div class="card px-3 py-2 mb-2"><h3 class="h4 mt-0 mb-2 sp-time" data-date="2026-09-20T00:00:00"></h3><div class="border-bottom"></div>` + day + `</div>`
	}
	return `<!DOCTYPE html><html><head><title>US Zoho Services Availability Status</title></head><body>
<div id="spContainer">` + banner + `
<section id="spComponentSummary"></section>
<section id="spIncidentHistory">` + cards + `</section>
</div></body></html>`
}

var resolvedStatusIQIncident = statusIQIncident(
	"Di-resolved==", "major-outage", `"Zoho Bugtracker" is Down`,
	"2026-09-20T05:54:17+0530", "2026-09-20T06:04:40+0530", "Zoho Bugtracker",
	statusIQRow("incident.state.14", "All components are Operational", "2026-09-20T06:04:40+0530"),
	statusIQRow("incident.state.10", `"Zoho Bugtracker" is Down`, "2026-09-20T05:54:17+0530"),
)

func TestParseStatusIQPageReadsTheIncidentHistory(t *testing.T) {
	entries, err := parseStatusIQPage(strings.NewReader(statusIQPage("", resolvedStatusIQIncident)), "https://us.zohostatus.com")
	if err != nil {
		t.Fatalf("parseStatusIQPage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	entry := entries[0]
	if entry.ID != "Di-resolved==" {
		t.Errorf("ID = %q", entry.ID)
	}
	if entry.Name != `"Zoho Bugtracker" is Down` {
		t.Errorf("Name = %q", entry.Name)
	}
	if entry.Kind != kindIncident || entry.Impact != "critical" || entry.Status != "resolved" {
		t.Errorf("kind/impact/status = %s/%s/%s, want incident/critical/resolved", entry.Kind, entry.Impact, entry.Status)
	}
	if entry.Shortlink != "https://us.zohostatus.com/incident/Di-resolved==" {
		t.Errorf("Shortlink = %q", entry.Shortlink)
	}
	if want := time.Date(2026, 9, 20, 0, 24, 17, 0, time.UTC); !entry.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %s, want %s", entry.CreatedAt, want)
	}

	updates := entry.updatesOldestFirst()
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	if updates[0].Status != "acknowledged" || updates[1].Status != "resolved" {
		t.Errorf("update order = %s then %s, want acknowledged then resolved", updates[0].Status, updates[1].Status)
	}
	if updates[1].Body != "All components are Operational" {
		t.Errorf("Body = %q", updates[1].Body)
	}
	if updates[0].ID == "" || updates[0].ID == updates[1].ID {
		t.Errorf("update ids must be present and distinct, got %q and %q", updates[0].ID, updates[1].ID)
	}
	if got := formatComponents(updates[0].AffectedComponents); got != "Zoho Bugtracker" {
		t.Errorf("components = %q", got)
	}
}

// StatusIQ gives updates no ids, so the derived one is all that stops a repost.
// It has to come out the same on every poll of an unchanged page.
func TestParseStatusIQPageDerivesStableUpdateIDs(t *testing.T) {
	page := statusIQPage("", resolvedStatusIQIncident)

	first, err := parseStatusIQPage(strings.NewReader(page), "https://us.zohostatus.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseStatusIQPage(strings.NewReader(page), "https://us.zohostatus.com")
	if err != nil {
		t.Fatal(err)
	}
	for index := range first[0].IncidentUpdates {
		if first[0].IncidentUpdates[index].ID != second[0].IncidentUpdates[index].ID {
			t.Errorf("update %d id changed between polls", index)
		}
	}
}

// An open incident also shows up outside the history list, and the one that
// spans midnight is listed under two days. Each must come out once, with the
// most complete set of updates.
func TestParseStatusIQPageMergesAnIncidentListedMoreThanOnce(t *testing.T) {
	acknowledged := statusIQRow("incident.state.10", "Mail is Down", "2026-09-20T23:50:00+0530")
	identified := statusIQRow("incident.state.12", "Root cause found", "2026-09-21T00:10:00+0530")

	partial := statusIQIncident("Di-open==", "partial-outage", "Mail is Down", "2026-09-20T23:50:00+0530", "", "Zoho Mail", acknowledged)
	full := statusIQIncident("Di-open==", "partial-outage", "Mail is Down", "2026-09-20T23:50:00+0530", "", "Zoho Mail", identified, acknowledged)

	entries, err := parseStatusIQPage(strings.NewReader(statusIQPage(`<section class="active">`+partial+`</section>`, full, partial)), "https://us.zohostatus.com")
	if err != nil {
		t.Fatalf("parseStatusIQPage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the one incident", len(entries))
	}
	if got := len(entries[0].IncidentUpdates); got != 2 {
		t.Errorf("got %d updates, want the complete copy's 2", got)
	}
	if entries[0].Status != "identified" || entries[0].Impact != "major" {
		t.Errorf("status/impact = %s/%s, want identified/major", entries[0].Status, entries[0].Impact)
	}
}

// Two incidents in the same day card must not be read as one.
func TestParseStatusIQPageKeepsNeighbouringIncidentsApart(t *testing.T) {
	other := statusIQIncident("Di-other==", "degraded-performance", "Desk is slow",
		"2026-09-20T07:00:00+0530", "2026-09-20T07:30:00+0530", "Zoho Desk",
		statusIQRow("incident.state.10", "Desk is slow", "2026-09-20T07:00:00+0530"),
	)

	entries, err := parseStatusIQPage(strings.NewReader(statusIQPage("", resolvedStatusIQIncident+other)), "https://us.zohostatus.com")
	if err != nil {
		t.Fatalf("parseStatusIQPage: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if len(entries[0].IncidentUpdates) != 2 || len(entries[1].IncidentUpdates) != 1 {
		t.Errorf("updates per entry = %d and %d, want 2 and 1", len(entries[0].IncidentUpdates), len(entries[1].IncidentUpdates))
	}
	if entries[1].Impact != "minor" {
		t.Errorf("Impact = %q, want minor", entries[1].Impact)
	}
}

func TestParseStatusIQPageTreatsMaintenanceStatesAsMaintenance(t *testing.T) {
	maintenance := statusIQIncident("Di-mnt==", "maintenance", "Database upgrade",
		"2026-09-20T01:00:00+0530", "2026-09-20T02:00:00+0530", "Zoho CRM",
		statusIQRow("maintenance.state.22", "Maintenance started", "2026-09-20T01:00:00+0530"),
		statusIQRow("maintenance.state.24", "Maintenance completed", "2026-09-20T02:00:00+0530"),
	)
	page := statusIQPage("", maintenance)

	entries, err := parseStatusIQPage(strings.NewReader(page), "https://us.zohostatus.com")
	if err != nil {
		t.Fatalf("parseStatusIQPage: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != kindMaintenance || entries[0].Status != "completed" {
		t.Fatalf("got %+v, want one completed maintenance", entries)
	}
	if !isTerminal(entries[0].Status) {
		t.Error("a completed maintenance should be terminal")
	}
}

// A page that has lost its history section has changed layout. Reporting that
// as "no incidents" would go silent exactly when the parser stops working.
func TestParseStatusIQPageFailsWithoutTheHistorySection(t *testing.T) {
	_, err := parseStatusIQPage(strings.NewReader(`<html><body><p>Something else</p></body></html>`), "https://us.zohostatus.com")
	if err == nil {
		t.Fatal("a page without the history section should be an error")
	}
}

func TestFetchStatusIQEntriesReadsTheFrontPage(t *testing.T) {
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		_, _ = w.Write([]byte(statusIQPage("", resolvedStatusIQIncident)))
	}))
	t.Cleanup(server.Close)

	entries, err := fetchStatusIQEntries(context.Background(), server.Client(), server.URL, true)
	if err != nil {
		t.Fatalf("fetchStatusIQEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if len(requested) != 1 || requested[0] != "/" {
		t.Errorf("requests = %v, want one GET of /", requested)
	}
	if !strings.HasPrefix(entries[0].Shortlink, server.URL+"/incident/") {
		t.Errorf("Shortlink = %q, want it on the polled host", entries[0].Shortlink)
	}
}

func TestFetchStatusIQEntriesCanSkipMaintenance(t *testing.T) {
	maintenance := statusIQIncident("Di-mnt==", "maintenance", "Database upgrade",
		"2026-09-20T01:00:00+0530", "", "Zoho CRM",
		statusIQRow("maintenance.state.22", "Maintenance started", "2026-09-20T01:00:00+0530"),
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(statusIQPage("", resolvedStatusIQIncident+maintenance)))
	}))
	t.Cleanup(server.Close)

	entries, err := fetchStatusIQEntries(context.Background(), server.Client(), server.URL, false)
	if err != nil {
		t.Fatalf("fetchStatusIQEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != kindIncident {
		t.Fatalf("got %d entries, want only the incident", len(entries))
	}
}

func TestFetchStatusIQEntriesReportsHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	if _, err := fetchStatusIQEntries(context.Background(), server.Client(), server.URL, true); err == nil {
		t.Fatal("a 502 should be an error")
	}
}

func TestLoadSettingsValidatesTheSource(t *testing.T) {
	t.Setenv("DISCORD_WEBHOOK_PARAMETER_NAME", "/webhook")
	t.Setenv("STATE_BUCKET", "bucket")

	t.Setenv("STATUS_PAGE_SOURCE", "")
	if current, err := loadSettings(); err != nil || current.source != sourceStatuspage {
		t.Errorf("unset source = %q, %v; want statuspage", current.source, err)
	}

	t.Setenv("STATUS_PAGE_SOURCE", "StatusIQ")
	if current, err := loadSettings(); err != nil || current.source != sourceStatusIQ {
		t.Errorf("StatusIQ source = %q, %v; want statusiq", current.source, err)
	}

	t.Setenv("STATUS_PAGE_SOURCE", "rss")
	if _, err := loadSettings(); err == nil {
		t.Error("an unknown source should be rejected")
	}
}
