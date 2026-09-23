package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	colorResolved    = 0x2ecc71
	colorCritical    = 0xe74c3c
	colorMajor       = 0xe67e22
	colorMinor       = 0xf1c40f
	colorMaintenance = 0x3498db
	colorNone        = 0x9aa0a6

	truncationSuffix = "..."
)

var statusLabels = map[string]string{
	// Incident lifecycle.
	"acknowledged":  "Acknowledged",
	"investigating": "Investigating",
	"identified":    "Identified",
	"monitoring":    "Monitoring",
	"observing":     "Observing",
	"resolved":      "Resolved",
	"postmortem":    "Postmortem",
	// Scheduled maintenance lifecycle.
	"scheduled":   "Scheduled",
	"in_progress": "In progress",
	"verifying":   "Verifying",
	"completed":   "Completed",
}

// terminalStatuses are the ones that close an entry out. They get the green
// embed and, for an incident with a role configured, the mention -- because an
// edit never notifies anyone on Discord and a follow-up inside a thread only
// reaches members who already joined it, so without a mention the "it is fixed"
// message is the one nobody sees. See shouldMention for why maintenance is left
// out of that.
var terminalStatuses = map[string]bool{
	"resolved":   true,
	"postmortem": true,
	"completed":  true,
}

func statusLabel(status string) string {
	if label, ok := statusLabels[strings.ToLower(strings.TrimSpace(status))]; ok {
		return label
	}
	if status == "" {
		return "Update"
	}
	// An unknown status is still worth showing verbatim rather than dropping.
	return strings.ToUpper(status[:1]) + status[1:]
}

func isTerminal(status string) bool {
	return terminalStatuses[strings.ToLower(strings.TrimSpace(status))]
}

// shouldMention decides whether this message carries the role mention. Only the
// post that opens a thread and the one that closes the entry out do, so the
// updates in between stay quiet -- and scheduled maintenance never does at all.
//
// Maintenance is announced days ahead and runs to a plan; nobody has to react
// to it the way an incident demands. Pinging a role for a window that was
// always going to happen is what trains people to ignore the ping, which then
// costs them the incident notification the mention exists for.
//
// A minor incident is left quiet for the same reason. Statuspage keeps no
// history of an incident's impact, only its current value, so this reads the
// impact as of the poll that delivers the update. Two consequences:
//
//   - An incident escalated past minor is mentioned on its resolution even if
//     it opened without one.
//   - One that already pinged keeps its resolution ping after being lowered to
//     minor, because entry.Mentioned remembers the first one. Only an incident
//     that opens, is downgraded and resolves between two polls -- so that it is
//     never seen above minor at all -- goes out without any mention.
func shouldMention(entry statusEntry, update statusUpdate, openThread bool, current settings) bool {
	if current.mentionRoleID == "" {
		return false
	}
	if entry.Kind == kindMaintenance {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(entry.Impact), "minor") && !entry.Mentioned {
		return false
	}
	return openThread || isTerminal(update.Status)
}

func embedColor(entry statusEntry, update statusUpdate) int {
	if isTerminal(update.Status) {
		return colorResolved
	}
	if entry.Kind == kindMaintenance {
		return colorMaintenance
	}
	switch strings.ToLower(strings.TrimSpace(entry.Impact)) {
	case "critical":
		return colorCritical
	case "major":
		return colorMajor
	case "minor":
		return colorMinor
	case "maintenance":
		return colorMaintenance
	}
	return colorNone
}

// buildPayload renders one Statuspage update as one Discord message.
//
// openThread makes it the forum post that starts the entry's thread; every
// later update for the same entry is a follow-up inside that thread.
func buildPayload(entry statusEntry, update statusUpdate, openThread bool, current settings) webhookPayload {
	label := statusLabel(update.Status)

	// The content line is what a push notification previews, so it carries the
	// status and the entry name rather than leaving them buried in the embed.
	content := fmt.Sprintf("**%s** — %s", label, entry.Name)
	mentions := allowedMentions{Parse: []string{}}
	if shouldMention(entry, update, openThread, current) {
		content = fmt.Sprintf("<@&%s> %s", current.mentionRoleID, content)
		mentions.Roles = []string{current.mentionRoleID}
	}

	rendered := embed{
		Title:       truncate(label, discordEmbedTitleLimit),
		URL:         strings.TrimSpace(entry.Shortlink),
		Description: truncate(normalizeBody(update.Body), discordEmbedDescriptionLimit),
		Color:       embedColor(entry, update),
		Footer:      &embedFooter{Text: truncate(current.pageHost, discordEmbedFooterTextLimit)},
	}
	if at := update.at(); !at.IsZero() {
		rendered.Timestamp = at.UTC().Format(time.RFC3339)
	}
	rendered.Fields = buildFields(entry, update)

	payload := webhookPayload{
		Content:         truncate(content, discordContentLimit),
		Username:        webhookUsername(current.pageLabel),
		Embeds:          []embed{rendered},
		AllowedMentions: mentions,
	}
	if openThread {
		payload.ThreadName = truncate(threadName(entry, current.pageLabel), discordThreadNameLimit)
	}

	return payload
}

// webhookUsername is what tells two status pages apart when they post into the
// same channel, so it carries the page label rather than a fixed name.
func webhookUsername(pageLabel string) string {
	if pageLabel == "" {
		return "Status"
	}
	return pageLabel + " Status"
}

func buildFields(entry statusEntry, update statusUpdate) []embedField {
	var fields []embedField

	if impact := strings.TrimSpace(entry.Impact); impact != "" && impact != "none" {
		fields = append(fields, embedField{
			Name:   "Impact",
			Value:  truncate(strings.ToUpper(impact[:1])+impact[1:], discordEmbedFieldValueLimit),
			Inline: true,
		})
	}

	if entry.Kind == kindMaintenance && entry.ScheduledFor != nil {
		window := entry.ScheduledFor.UTC().Format("2006-01-02 15:04 MST")
		if entry.ScheduledUntil != nil {
			window += " – " + entry.ScheduledUntil.UTC().Format("2006-01-02 15:04 MST")
		}
		fields = append(fields, embedField{
			Name:   "Window",
			Value:  truncate(window, discordEmbedFieldValueLimit),
			Inline: true,
		})
	}

	if components := formatComponents(update.AffectedComponents); components != "" {
		fields = append(fields, embedField{
			Name:  truncate("Affected components", discordEmbedFieldNameLimit),
			Value: truncate(components, discordEmbedFieldValueLimit),
		})
	}

	return fields
}

func formatComponents(components []affectedComponent) string {
	if len(components) == 0 {
		return ""
	}

	// Only the components whose status actually moved in this update: an
	// unchanged one carries no information and Statuspage lists every component
	// it touched, changed or not.
	changed := make([]string, 0, len(components))
	for _, component := range components {
		if component.OldStatus == component.NewStatus {
			continue
		}
		changed = append(changed, fmt.Sprintf("%s: %s → %s",
			strings.TrimSpace(component.Name),
			componentStatusLabel(component.OldStatus),
			componentStatusLabel(component.NewStatus),
		))
	}
	if len(changed) == 0 {
		names := make([]string, 0, len(components))
		for _, component := range components {
			names = append(names, strings.TrimSpace(component.Name))
		}
		sort.Strings(names)
		return strings.Join(names, ", ")
	}

	sort.Strings(changed)
	return strings.Join(changed, "\n")
}

func componentStatusLabel(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "unknown"
	}
	return strings.ReplaceAll(status, "_", " ")
}

func threadName(entry statusEntry, pageLabel string) string {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = "Status update"
	}
	if entry.Kind == kindMaintenance {
		name = "Maintenance: " + name
	}
	// Dated, because forum posts for two separate incidents with the same
	// Statuspage title are otherwise indistinguishable in the channel list.
	if !entry.CreatedAt.IsZero() {
		name = entry.CreatedAt.UTC().Format("2006-01-02") + " " + name
	}
	// Prefixed with the page, because several status pages can be pointed at
	// one channel and an incident title rarely names the service it belongs
	// to: GitHub calls one "Incident with GitHub.com" but another just
	// "Incident with Actions".
	if pageLabel != "" {
		name = pageLabel + " · " + name
	}
	return name
}

// normalizeBody folds the CRLF line endings Statuspage sends and collapses the
// runs of blank lines they leave behind.
func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	for strings.Contains(body, "\n\n\n") {
		body = strings.ReplaceAll(body, "\n\n\n", "\n\n")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "(no details provided)"
	}
	return body
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	suffixLength := utf8.RuneCountInString(truncationSuffix)
	if limit <= suffixLength {
		return string([]rune(truncationSuffix)[:limit])
	}
	runes := []rune(value)
	return string(runes[:limit-suffixLength]) + truncationSuffix
}
