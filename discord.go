package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	discordContentLimit          = 2000
	discordThreadNameLimit       = 100
	discordEmbedTitleLimit       = 256
	discordEmbedDescriptionLimit = 4096
	discordEmbedFieldNameLimit   = 256
	discordEmbedFieldValueLimit  = 1024
	discordEmbedFooterTextLimit  = 2048

	discordMaxAttempts = 3
	discordMaxBackoff  = 5 * time.Second
)

type webhookPayload struct {
	Content  string `json:"content,omitempty"`
	Username string `json:"username,omitempty"`
	// ThreadName opens a new forum post. Discord only accepts it when the
	// webhook's channel is a forum or media channel; in a plain text channel a
	// webhook cannot create a thread at all, which is why this notifier
	// requires a forum channel.
	ThreadName      string          `json:"thread_name,omitempty"`
	Embeds          []embed         `json:"embeds,omitempty"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
}

type embed struct {
	Title       string       `json:"title,omitempty"`
	URL         string       `json:"url,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []embedField `json:"fields,omitempty"`
	Footer      *embedFooter `json:"footer,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type embedFooter struct {
	Text string `json:"text"`
}

type webhookMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
}

// threadID reports the thread the message landed in. For a forum post created
// through thread_name, Discord answers with the starter message, whose
// channel_id is the new thread -- the message id happens to match today, but
// channel_id is the field that is actually specified to hold it.
func (m webhookMessage) threadID() string {
	if m.ChannelID != "" {
		return m.ChannelID
	}
	return m.ID
}

type rateLimitResponse struct {
	RetryAfter float64 `json:"retry_after"`
}

// discordError carries the HTTP status through to the caller, which needs to
// tell a 404 -- the thread was deleted, so a replacement has to be opened --
// apart from every other rejection.
type discordError struct {
	StatusCode int
	Body       string
}

func (e *discordError) Error() string {
	return fmt.Sprintf("Discord returned %d: %s", e.StatusCode, e.Body)
}

// postWebhook executes the webhook and returns the created message.
//
// threadID selects an existing thread; leave it empty and set payload.ThreadName
// instead to open a new one. wait=true is always set, because the message body
// is the only place the new thread's id is reported.
func (a *app) postWebhook(ctx context.Context, webhookURL, threadID string, payload webhookPayload) (webhookMessage, error) {
	endpoint, err := url.Parse(webhookURL)
	if err != nil {
		return webhookMessage{}, err
	}
	query := endpoint.Query()
	query.Set("wait", "true")
	if threadID != "" {
		query.Set("thread_id", threadID)
	}
	endpoint.RawQuery = query.Encode()

	body, err := json.Marshal(payload)
	if err != nil {
		return webhookMessage{}, err
	}

	var lastErr error
	for attempt := 1; attempt <= discordMaxAttempts; attempt++ {
		message, retryAfter, err := a.postWebhookOnce(ctx, endpoint.String(), body)
		if err == nil {
			return message, nil
		}
		lastErr = err
		if retryAfter <= 0 || attempt == discordMaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return webhookMessage{}, ctx.Err()
		case <-time.After(retryAfter):
		}
	}

	return webhookMessage{}, lastErr
}

// postWebhookOnce returns a positive retryAfter when the failure is worth
// retrying: a 429 from Discord's own rate limiter, or a 5xx.
func (a *app) postWebhookOnce(ctx context.Context, endpoint string, body []byte) (webhookMessage, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return webhookMessage{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return webhookMessage{}, 0, err
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return webhookMessage{}, 0, fmt.Errorf("read Discord response: %w", readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var message webhookMessage
		if err := json.Unmarshal(responseBody, &message); err != nil {
			return webhookMessage{}, 0, fmt.Errorf("decode Discord response: %w", err)
		}
		return message, 0, nil
	}

	failure := &discordError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return webhookMessage{}, retryAfterDelay(responseBody), failure
	case resp.StatusCode >= 500:
		return webhookMessage{}, time.Second, failure
	}

	return webhookMessage{}, 0, failure
}

func retryAfterDelay(body []byte) time.Duration {
	var limited rateLimitResponse
	if err := json.Unmarshal(body, &limited); err != nil || limited.RetryAfter <= 0 {
		return time.Second
	}
	delay := time.Duration(limited.RetryAfter * float64(time.Second))
	if delay > discordMaxBackoff {
		return discordMaxBackoff
	}
	return delay
}

func parseWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("parameter is empty")
	}

	var value string
	if err := json.Unmarshal([]byte(raw), &value); err == nil && strings.TrimSpace(value) != "" {
		return validateWebhookURL(strings.TrimSpace(value))
	}

	var object map[string]string
	if err := json.Unmarshal([]byte(raw), &object); err == nil {
		for _, key := range []string{"url", "webhook_url", "discord_webhook_url"} {
			if value := strings.TrimSpace(object[key]); value != "" {
				return validateWebhookURL(value)
			}
		}
	}

	return validateWebhookURL(raw)
}

func validateWebhookURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("webhook URL must be an absolute https URL")
	}
	return raw, nil
}
