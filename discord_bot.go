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

// renameThread rewrites a thread's title.
//
// This is the one thing a webhook cannot do. thread_name is only accepted when
// the post is created, and PATCH /channels/{id} needs a real identity, so the
// running phase marker in a thread's title costs a bot token that nothing else
// here needs. Everything else still goes through the webhook.
//
// Renames are rate limited far more tightly than messages -- two per ten minutes
// for a given channel -- which is why the caller only does this when the phase
// actually changes, not on every update.
func (a *app) renameThread(ctx context.Context, botToken, threadID, title string) error {
	body, err := json.Marshal(map[string]string{"name": title})
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
		retryAfter, err := a.renameThreadOnce(ctx, endpoint, botToken, body)
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

func (a *app) renameThreadOnce(ctx context.Context, endpoint, botToken string, body []byte) (time.Duration, error) {
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
