package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A trimmed copy of the real response shape, including display_at and the
// newest-first ordering Statuspage serves.
const incidentsFeed = `{
  "page": {"id": "tymt9n04zgry", "name": "Claude"},
  "incidents": [
    {
      "id": "qt14v73myyy5",
      "name": "Service disruption on Claude services",
      "status": "resolved",
      "impact": "critical",
      "shortlink": "https://stspg.io/cc6kmwrqhzmh",
      "created_at": "2026-08-16T21:58:56.949Z",
      "updated_at": "2026-08-16T22:34:39.051Z",
      "resolved_at": "2026-08-16T22:34:39.037Z",
      "incident_updates": [
        {
          "id": "1jwtc692sr18",
          "status": "resolved",
          "body": "The issue has been resolved.",
          "created_at": "2026-08-16T22:34:39.037Z",
          "display_at": "2026-08-16T22:34:39.037Z",
          "affected_components": [
            {"code": "rwppv331jlwc", "name": "claude.ai", "old_status": "major_outage", "new_status": "operational"}
          ]
        },
        {
          "id": "4mynwszw5syj",
          "status": "investigating",
          "body": "We are investigating an issue.",
          "created_at": "2026-08-16T21:58:57.096Z",
          "display_at": "2026-08-16T21:58:57.096Z",
          "affected_components": []
        }
      ]
    }
  ]
}`

const maintenancesFeed = `{
  "page": {"id": "tymt9n04zgry", "name": "Claude"},
  "scheduled_maintenances": [
    {
      "id": "mnt-1",
      "name": "Planned maintenance",
      "status": "scheduled",
      "impact": "maintenance",
      "created_at": "2026-08-15T00:00:00.000Z",
      "updated_at": "2026-08-15T00:00:00.000Z",
      "scheduled_for": "2026-08-20T02:00:00.000Z",
      "scheduled_until": "2026-08-20T04:00:00.000Z",
      "incident_updates": [
        {
          "id": "mu-1",
          "status": "scheduled",
          "body": "Maintenance is planned.",
          "created_at": "2026-08-15T00:00:00.000Z",
          "display_at": "2026-08-15T00:00:00.000Z"
        }
      ]
    }
  ]
}`

func statusServer(t *testing.T, incidentsStatus, maintenancesStatus int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/incidents.json"):
			w.WriteHeader(incidentsStatus)
			if incidentsStatus == http.StatusOK {
				_, _ = w.Write([]byte(incidentsFeed))
			}
		case strings.HasSuffix(r.URL.Path, "/scheduled-maintenances.json"):
			w.WriteHeader(maintenancesStatus)
			if maintenancesStatus == http.StatusOK {
				_, _ = w.Write([]byte(maintenancesFeed))
			}
		default:
			t.Errorf("unexpected request for %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func TestFetchEntriesMergesBothFeedsOldestFirst(t *testing.T) {
	server := statusServer(t, http.StatusOK, http.StatusOK)

	entries, err := fetchEntries(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("fetchEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	// The maintenance was created a day earlier, so it sorts first.
	if entries[0].Kind != kindMaintenance || entries[1].Kind != kindIncident {
		t.Fatalf("entries are not oldest first: %s then %s", entries[0].Kind, entries[1].Kind)
	}
	if entries[0].ScheduledFor == nil || entries[0].ScheduledUntil == nil {
		t.Error("the maintenance window should be decoded")
	}

	// The feed lists updates newest first; posting order is the opposite.
	updates := entries[1].updatesOldestFirst()
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	if updates[0].Status != "investigating" || updates[1].Status != "resolved" {
		t.Errorf("update order = %s then %s, want investigating then resolved", updates[0].Status, updates[1].Status)
	}
	if got := updates[0].AffectedComponents; len(got) != 0 {
		t.Errorf("affected_components = %v, want empty", got)
	}
}

// One feed failing must not discard the other: an outage on the maintenance
// endpoint cannot be allowed to hold up incident notifications.
func TestFetchEntriesKeepsTheReadableFeedWhenTheOtherFails(t *testing.T) {
	server := statusServer(t, http.StatusOK, http.StatusInternalServerError)

	entries, err := fetchEntries(context.Background(), server.Client(), server.URL)
	if err == nil {
		t.Fatal("fetchEntries should report the failed feed")
	}
	if !strings.Contains(err.Error(), "scheduled maintenances feed") {
		t.Errorf("error = %v, want it to name the failed feed", err)
	}
	if len(entries) != 1 || entries[0].Kind != kindIncident {
		t.Fatalf("got %d entries, want the one incident", len(entries))
	}
}

// display_at is the timestamp the status page itself shows, so a backdated
// update has to be aged by that rather than by created_at.
func TestUpdateTimePrefersDisplayAt(t *testing.T) {
	created := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	displayed := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

	if got := (statusUpdate{CreatedAt: created, DisplayAt: displayed}).at(); !got.Equal(displayed) {
		t.Errorf("at() = %s, want display_at %s", got, displayed)
	}
	if got := (statusUpdate{CreatedAt: created}).at(); !got.Equal(created) {
		t.Errorf("at() = %s, want created_at %s when display_at is absent", got, created)
	}
}
