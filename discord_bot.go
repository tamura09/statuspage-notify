package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const discordAPIBase = "https://discord.com/api/v10"

// closeThread archives a forum post, which is what Discord's "Close Post" does.
//
// This is the one thing a webhook cannot do: PATCH /channels/{id} needs a real
// identity, so closing a post costs a bot token that nothing else here needs.
// Every message still goes through the webhook.
//
// Archived, never locked. Statuspage can append a postmortem after the
// resolution, and posting to an archived thread reopens it by itself, so an
// archived post accepts that late update and is closed again. A locked one
// would reject it: a webhook carries no permission to post through a lock.
//
// Channel edits are rate limited far more tightly than messages -- twice per ten
// minutes for a given thread -- which is why the caller only does this on the
// transition, not on every poll of an already-resolved incident.
func (a *app) closeThread(ctx context.Context, botToken, threadID string) error {
	body, err := json.Marshal(map[string]bool{"archived": true})
	if err != nil {
		return err
	}

	base := a.botAPIBase
	if base == "" {
		base = discordAPIBase
	}
	endpoint := fmt.Sprintf("%s/channels/%s", strings.TrimRight(base, "/"), threadID)

	var lastErr error
	for attempt := 1; attempt <= discordMaxAttempts; attempt++ {
		retryAfter, err := a.patchChannelOnce(ctx, endpoint, botToken, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if retryAfter <= 0 || attempt == discordMaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
	}

	return lastErr
}

func (a *app) patchChannelOnce(ctx context.Context, endpoint, botToken string, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+botToken)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return 0, fmt.Errorf("read Discord response: %w", readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0, nil
	}

	failure := &discordError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return retryAfterDelay(responseBody), failure
	case resp.StatusCode >= 500:
		return time.Second, failure
	}

	return 0, failure
}
