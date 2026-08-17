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
	webhookUsername  = "Claude Status"
	footerText       = "status.claude.com"
)

var statusLabels = map[string]string{
	// Incident lifecycle.
	"investigating": "Investigating",
	"identified":    "Identified",
	"monitoring":    "Monitoring",
	"resolved":      "Resolved",
	"postmortem":    "Postmortem",
	// Scheduled maintenance lifecycle.
	"scheduled":   "Scheduled",
	"in_progress": "In progress",
	"verifying":   "Verifying",
	"completed":   "Completed",
}

// terminalStatuses are the ones that close an entry out. They get the green
// embed and, when one is configured, the role mention -- because an edit never
// notifies anyone on Discord and a follow-up inside a thread only reaches
// members who already joined it, so without a mention the "it is fixed" message
// is the one nobody sees.
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
func buildPayload(entry statusEntry, update statusUpdate, openThread bool, mentionRoleID string) webhookPayload {
	label := statusLabel(update.Status)

	// The content line is what a push notification previews, so it carries the
	// status and the entry name rather than leaving them buried in the embed.
	content := fmt.Sprintf("**%s** — %s", label, entry.Name)
	mentions := allowedMentions{Parse: []string{}}
	if mentionRoleID != "" && (openThread || isTerminal(update.Status)) {
		content = fmt.Sprintf("<@&%s> %s", mentionRoleID, content)
		mentions.Roles = []string{mentionRoleID}
	}

	rendered := embed{
		Title:       truncate(label, discordEmbedTitleLimit),
		URL:         strings.TrimSpace(entry.Shortlink),
		Description: truncate(normalizeBody(update.Body), discordEmbedDescriptionLimit),
		Color:       embedColor(entry, update),
		Footer:      &embedFooter{Text: truncate(footerText, discordEmbedFooterTextLimit)},
	}
	if at := update.at(); !at.IsZero() {
		rendered.Timestamp = at.UTC().Format(time.RFC3339)
	}
	rendered.Fields = buildFields(entry, update)

	payload := webhookPayload{
		Content:         truncate(content, discordContentLimit),
		Username:        webhookUsername,
		Embeds:          []embed{rendered},
		AllowedMentions: mentions,
	}
	if openThread {
		payload.ThreadName = truncate(threadName(entry), discordThreadNameLimit)
	}

	return payload
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

func threadName(entry statusEntry) string {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = "Claude status update"
	}
	if entry.Kind == kindMaintenance {
		name = "Maintenance: " + name
	}
	// Dated, because forum posts for two separate incidents with the same
	// Statuspage title are otherwise indistinguishable in the channel list.
	if !entry.CreatedAt.IsZero() {
		name = entry.CreatedAt.UTC().Format("2006-01-02") + " " + name
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
