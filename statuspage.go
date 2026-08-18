package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type entryKind string

const (
	kindIncident    entryKind = "incident"
	kindMaintenance entryKind = "maintenance"
)

// feedResponse covers both endpoints this poller reads. Each one populates
// exactly one of the two slices, so a single type decodes both.
type feedResponse struct {
	Incidents             []statusEntry `json:"incidents"`
	ScheduledMaintenances []statusEntry `json:"scheduled_maintenances"`
}

type statusEntry struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Status          string         `json:"status"`
	Impact          string         `json:"impact"`
	Shortlink       string         `json:"shortlink"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	ScheduledFor    *time.Time     `json:"scheduled_for"`
	ScheduledUntil  *time.Time     `json:"scheduled_until"`
	IncidentUpdates []statusUpdate `json:"incident_updates"`

	// Set by the fetcher from the endpoint the entry came from; Statuspage
	// serves incidents and maintenances through the same shape and nothing in
	// the payload itself distinguishes them.
	Kind entryKind `json:"-"`
}

type statusUpdate struct {
	ID                 string              `json:"id"`
	Status             string              `json:"status"`
	Body               string              `json:"body"`
	CreatedAt          time.Time           `json:"created_at"`
	DisplayAt          time.Time           `json:"display_at"`
	AffectedComponents []affectedComponent `json:"affected_components"`
}

type affectedComponent struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"`
}

// at reports the time the update should be attributed to. Statuspage lets an
// operator backdate an update through display_at, and that is the timestamp the
// status page itself shows, so it is also the one the age cutoff has to use.
func (u statusUpdate) at() time.Time {
	if !u.DisplayAt.IsZero() {
		return u.DisplayAt
	}
	return u.CreatedAt
}

// updatesOldestFirst returns the entry's updates in posting order. Statuspage
// serves them newest first, which is the reverse of the order a thread reads in.
func (e statusEntry) updatesOldestFirst() []statusUpdate {
	updates := append([]statusUpdate(nil), e.IncidentUpdates...)
	sort.SliceStable(updates, func(i, j int) bool {
		return updates[i].at().Before(updates[j].at())
	})
	return updates
}

// fetchEntries reads both feeds and merges them.
//
// summary.json is deliberately not used: it carries only unresolved incidents,
// so an incident disappears from it the moment it is resolved -- which is the
// one update this notifier most needs to deliver. incidents.json keeps the
// recent history, resolved entries included.
//
// A failure on one feed does not discard the other. The caller still gets the
// entries that were readable along with the error, so a maintenance feed
// outage cannot block incident notifications.
//
// includeMaintenance is false for pages whose maintenance feed is noise rather
// than news: Cloudflare posts a scheduled window per datacenter, which was
// eighteen entries in a day against five real incidents. The feed is not
// fetched at all in that case, so there is nothing to leak through later.
func fetchEntries(ctx context.Context, client *http.Client, baseURL string, includeMaintenance bool) ([]statusEntry, error) {
	var entries []statusEntry
	var problems []error

	incidents, err := fetchFeed(ctx, client, baseURL, "incidents.json")
	if err != nil {
		problems = append(problems, fmt.Errorf("incidents feed: %w", err))
	} else {
		entries = append(entries, tagKind(incidents.Incidents, kindIncident)...)
	}

	if includeMaintenance {
		maintenances, err := fetchFeed(ctx, client, baseURL, "scheduled-maintenances.json")
		if err != nil {
			problems = append(problems, fmt.Errorf("scheduled maintenances feed: %w", err))
		} else {
			entries = append(entries, tagKind(maintenances.ScheduledMaintenances, kindMaintenance)...)
		}
	}

	// Oldest first, so that when several incidents are opened between two polls
	// their threads appear in the order they started.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	return entries, joinErrors(problems)
}

func fetchFeed(ctx context.Context, client *http.Client, baseURL, path string) (feedResponse, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/" + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return feedResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-status-notify")

	resp, err := client.Do(req)
	if err != nil {
		return feedResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return feedResponse{}, fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var feed feedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&feed); err != nil {
		return feedResponse{}, fmt.Errorf("decode %s: %w", path, err)
	}

	return feed, nil
}

func tagKind(entries []statusEntry, kind entryKind) []statusEntry {
	for index := range entries {
		entries[index].Kind = kind
	}
	return entries
}
