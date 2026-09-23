package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// StatusIQ (Site24x7) is what Zoho runs its status pages on. It has no public
// JSON API: incidents.json on a StatusIQ host answers with the HTML page. What
// it does have is the page itself, which renders the last seven days of
// incidents server side, each with its full update history and ISO 8601
// timestamps in data attributes. One GET of the front page therefore carries
// everything the Statuspage poller reads from two feeds, and this file turns
// that markup into the same statusEntry values the rest of the function uses.
//
// The markup is not a contract, so the parser is strict about the one thing it
// cannot do without: if the incident history section is missing, the run
// fails loudly rather than reporting an empty page as "no incidents".

const statusIQHistorySectionID = "spIncidentHistory"

// statusIQStates maps the i18n keys StatusIQ renders an update's state as to
// the lowercase status names render.go already understands. The numbers are
// the page's own; their English labels come from its i18ncustomsp_en bundle.
var statusIQStates = map[string]string{
	"incident.state.10":    "acknowledged",
	"incident.state.11":    "investigating",
	"incident.state.12":    "identified",
	"incident.state.13":    "observing",
	"incident.state.14":    "resolved",
	"maintenance.state.21": "scheduled",
	"maintenance.state.22": "in_progress",
	"maintenance.state.23": "observing",
	"maintenance.state.24": "completed",
}

// statusIQImpacts maps the severity class on an incident's title link to the
// Statuspage impact names embedColor already colours.
var statusIQImpacts = map[string]string{
	"major-outage":         "critical",
	"partial-outage":       "major",
	"degraded-performance": "minor",
	"informational":        "none",
	"maintenance":          "maintenance",
	"under-maintenance":    "maintenance",
}

var statusIQIncidentID = regexp.MustCompile(`openNew\('([^']+)'\)`)

const statusIQTimeLayout = "2006-01-02T15:04:05-0700"

func fetchStatusIQEntries(ctx context.Context, client *http.Client, baseURL string, includeMaintenance bool) ([]statusEntry, error) {
	pageURL := strings.TrimRight(baseURL, "/") + "/"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "claude-status-notify")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s returned %d: %s", pageURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	entries, err := parseStatusIQPage(io.LimitReader(resp.Body, 8<<20), baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", pageURL, err)
	}

	if !includeMaintenance {
		entries = withoutKind(entries, kindMaintenance)
	}
	return entries, nil
}

func parseStatusIQPage(body io.Reader, baseURL string) ([]statusEntry, error) {
	document, err := html.Parse(body)
	if err != nil {
		return nil, err
	}

	section := findNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && attr(node, "id") == statusIQHistorySectionID
	})
	if section == nil {
		return nil, errors.New("incident history section not found; the page layout has probably changed")
	}

	// The whole document is scanned, not just the history section: an
	// incident that is still open is also shown in a banner above the component
	// list, which is absent whenever nothing is happening and so has never been
	// seen to pin its markup down. Anything that opens an incident and has
	// update rows next to it counts, wherever it sits.
	//
	// Keyed by incident id, because the same incident then appears more than
	// once; the copy with the most updates is the most complete one.
	byID := map[string]statusEntry{}
	var order []string

	for _, title := range findAll(document, isStatusIQIncidentTitle) {
		entry, ok := parseStatusIQIncident(title, baseURL)
		if !ok {
			continue
		}
		existing, seen := byID[entry.ID]
		if !seen {
			order = append(order, entry.ID)
		}
		if !seen || len(entry.IncidentUpdates) > len(existing.IncidentUpdates) {
			byID[entry.ID] = entry
		}
	}

	entries := make([]statusEntry, 0, len(order))
	for _, id := range order {
		entries = append(entries, byID[id])
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	return entries, nil
}

// parseStatusIQIncident reads one incident out of the history list. The title
// link carries the id and severity; everything else lives in the element that
// wraps it.
func parseStatusIQIncident(title *html.Node, baseURL string) (statusEntry, bool) {
	match := statusIQIncidentID.FindStringSubmatch(attr(title, "onclick"))
	if match == nil {
		return statusEntry{}, false
	}
	container := statusIQIncidentContainer(title, match[1])
	if container == nil {
		return statusEntry{}, false
	}

	entry := statusEntry{
		ID:        match[1],
		Name:      textContent(title),
		Shortlink: strings.TrimRight(baseURL, "/") + "/incident/" + match[1],
		Kind:      kindIncident,
	}
	for _, class := range strings.Fields(attr(title, "class")) {
		if impact, ok := statusIQImpacts[class]; ok {
			entry.Impact = impact
			break
		}
	}

	if rangeNode := findNode(container, func(node *html.Node) bool {
		return node.Type == html.ElementNode && hasClass(node, "incident-range")
	}); rangeNode != nil {
		// data-range="<start>','<end>" -- the quotes are the page's own, left
		// over from splicing the pair into a JavaScript call.
		start, _, _ := strings.Cut(attr(rangeNode, "data-range"), "'")
		if parsed, err := time.Parse(statusIQTimeLayout, strings.TrimSpace(start)); err == nil {
			entry.CreatedAt = parsed
		}
	}

	components := statusIQAffectedComponents(container)

	for _, row := range findAll(container, isStatusIQUpdateRow) {
		update, isMaintenance, ok := parseStatusIQUpdate(row)
		if !ok {
			continue
		}
		if isMaintenance {
			entry.Kind = kindMaintenance
		}
		update.AffectedComponents = components
		entry.IncidentUpdates = append(entry.IncidentUpdates, update)
	}

	if len(entry.IncidentUpdates) == 0 {
		return statusEntry{}, false
	}

	latest := entry.IncidentUpdates[0]
	for _, update := range entry.IncidentUpdates[1:] {
		if update.CreatedAt.After(latest.CreatedAt) {
			latest = update
		}
	}
	entry.Status = latest.Status
	entry.UpdatedAt = latest.CreatedAt
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = entry.updatesOldestFirst()[0].CreatedAt
	}

	return entry, true
}

func isStatusIQIncidentTitle(node *html.Node) bool {
	return node.Type == html.ElementNode && statusIQIncidentID.MatchString(attr(node, "onclick"))
}

// statusIQIncidentContainer climbs from an incident's title to the smallest
// ancestor that also holds its update rows. It gives up at the first ancestor
// that reaches another incident's title, so two incidents are never merged.
func statusIQIncidentContainer(title *html.Node, id string) *html.Node {
	for candidate := title.Parent; candidate != nil; candidate = candidate.Parent {
		for _, other := range findAll(candidate, isStatusIQIncidentTitle) {
			if match := statusIQIncidentID.FindStringSubmatch(attr(other, "onclick")); match[1] != id {
				return nil
			}
		}
		if findNode(candidate, isStatusIQUpdateRow) != nil {
			return candidate
		}
	}
	return nil
}

func isStatusIQUpdateRow(node *html.Node) bool {
	return node.Type == html.ElementNode && hasClass(node, "update-row")
}

func parseStatusIQUpdate(row *html.Node) (statusUpdate, bool, bool) {
	var update statusUpdate
	var stateKey string

	if stateNode := findNode(row, func(node *html.Node) bool {
		return node.Type == html.ElementNode && hasClass(node, "txt-bold") && attr(node, "data-i18n") != ""
	}); stateNode != nil {
		stateKey = attr(stateNode, "data-i18n")
	}
	status, ok := statusIQStates[stateKey]
	if !ok {
		// An unmapped key is still an update; show its tail rather than drop it.
		status = stateKey[strings.LastIndex(stateKey, ".")+1:]
	}
	update.Status = status

	if bodyNode := findNode(row, func(node *html.Node) bool {
		return node.Type == html.ElementNode && hasClass(node, "formatted-content")
	}); bodyNode != nil {
		update.Body = textContent(bodyNode)
	}

	if dateNode := findNode(row, func(node *html.Node) bool {
		return node.Type == html.ElementNode && attr(node, "data-date") != ""
	}); dateNode != nil {
		if parsed, err := time.Parse(statusIQTimeLayout, attr(dateNode, "data-date")); err == nil {
			update.CreatedAt = parsed
		}
	}
	if update.CreatedAt.IsZero() {
		// Without a time the update can neither be ordered nor aged out, and
		// posting it anyway risks replaying history on every layout change.
		return statusUpdate{}, false, false
	}

	// StatusIQ gives updates no id of their own, so one is derived from what
	// identifies an update on the page: its state, its time and its text.
	sum := sha256.Sum256([]byte(stateKey + "\x00" + update.CreatedAt.UTC().Format(time.RFC3339) + "\x00" + update.Body))
	update.ID = hex.EncodeToString(sum[:8])

	return update, strings.HasPrefix(stateKey, "maintenance."), true
}

// statusIQAffectedComponents reads the paragraph that follows the "affected
// components" heading. StatusIQ gives only names, not per-component status
// changes, so they come back unchanged and render as a plain list.
func statusIQAffectedComponents(container *html.Node) []affectedComponent {
	heading := findNode(container, func(node *html.Node) bool {
		return node.Type == html.ElementNode && attr(node, "data-i18n") == "incident.affected.components"
	})
	if heading == nil {
		return nil
	}
	for sibling := heading.NextSibling; sibling != nil; sibling = sibling.NextSibling {
		if sibling.Type != html.ElementNode {
			continue
		}
		var components []affectedComponent
		for _, name := range strings.Split(textContent(sibling), ",") {
			if name = strings.TrimSpace(name); name != "" {
				components = append(components, affectedComponent{Name: name})
			}
		}
		return components
	}
	return nil
}

func withoutKind(entries []statusEntry, kind entryKind) []statusEntry {
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Kind != kind {
			kept = append(kept, entry)
		}
	}
	return kept
}

func findNode(root *html.Node, match func(*html.Node) bool) *html.Node {
	if match(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findNode(child, match); found != nil {
			return found
		}
	}
	return nil
}

func findAll(root *html.Node, match func(*html.Node) bool) []*html.Node {
	var found []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if match(node) {
			found = append(found, node)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return found
}

func attr(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}

func hasClass(node *html.Node, class string) bool {
	for _, value := range strings.Fields(attr(node, "class")) {
		if value == class {
			return true
		}
	}
	return false
}

// textContent flattens a node to text, turning <br> and block boundaries into
// line breaks and collapsing the indentation whitespace the page is full of.
func textContent(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		switch current.Type {
		case html.TextNode:
			builder.WriteString(current.Data)
		case html.ElementNode:
			switch current.Data {
			case "script", "style":
				return
			case "br":
				builder.WriteString("\n")
				return
			case "p", "div", "li":
				builder.WriteString("\n")
				defer builder.WriteString("\n")
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)

	lines := strings.Split(builder.String(), "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		cleaned = append(cleaned, strings.Join(strings.Fields(line), " "))
	}
	text := strings.Join(cleaned, "\n")
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}
