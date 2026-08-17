package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	defaultStatusPageBaseURL = "https://status.claude.com/api/v2"
	defaultStateKey          = "claude-status/state.json"
	defaultMaxUpdateAge      = 24 * time.Hour
	defaultStateRetention    = 30 * 24 * time.Hour
)

type app struct {
	parameters *ssm.Client
	objects    *s3.Client
	httpClient *http.Client
	now        func() time.Time
}

type settings struct {
	baseURL              string
	webhookParameterName string
	stateBucket          string
	stateKey             string
	mentionRoleID        string
	maxUpdateAge         time.Duration
	stateRetention       time.Duration
}

func main() {
	ctx := context.Background()
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}

	lambda.Start((&app{
		parameters: ssm.NewFromConfig(awsConfig),
		objects:    s3.NewFromConfig(awsConfig),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		now: time.Now,
	}).handle)
}

func (a *app) handle(ctx context.Context) error {
	current, err := loadSettings()
	if err != nil {
		return err
	}

	rawWebhook, err := a.parameterString(ctx, current.webhookParameterName)
	if err != nil {
		return fmt.Errorf("read Discord webhook parameter: %w", err)
	}
	webhookURL, err := parseWebhookURL(rawWebhook)
	if err != nil {
		return fmt.Errorf("parse Discord webhook parameter: %w", err)
	}

	state, err := a.loadState(ctx, current.stateBucket, current.stateKey)
	if err != nil {
		return err
	}

	entries, fetchErr := fetchEntries(ctx, a.httpClient, current.baseURL)

	now := a.now()
	posted, deliverErr := a.deliver(ctx, webhookURL, current, entries, state, now)
	state.prune(now, current.stateRetention)

	// Written even when delivery failed part way through, so the updates that
	// did reach Discord are not posted a second time on the next run.
	saveErr := a.saveState(ctx, current.stateBucket, current.stateKey, state)

	log.Printf("checked %d entries, posted %d updates", len(entries), posted)

	return joinErrors([]error{fetchErr, deliverErr, saveErr})
}

// deliver posts every update that has not been posted yet, oldest first, into
// the Discord thread belonging to its Statuspage entry.
func (a *app) deliver(ctx context.Context, webhookURL string, current settings, entries []statusEntry, state *notifierState, now time.Time) (int, error) {
	cutoff := now.Add(-current.maxUpdateAge)
	posted := 0
	var problems []error

	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}

		for _, update := range entry.updatesOldestFirst() {
			if strings.TrimSpace(update.ID) == "" || state.hasPosted(entry.ID, update.ID) {
				continue
			}

			// Older than the cutoff: marked as seen without being posted. This
			// is what keeps the first run after a deployment -- and any run
			// after the function has been failing for a while -- from replaying
			// days of already-resolved incidents into the channel.
			if update.at().Before(cutoff) {
				state.record(entry, update.ID, update.at())
				continue
			}

			threadID := state.Entries[entry.ID].ThreadID
			message, err := a.postUpdate(ctx, webhookURL, current, entry, update, threadID)
			if err != nil {
				problems = append(problems, fmt.Errorf("post entry %s update %s: %w", entry.ID, update.ID, err))
				// Stop at the first failure for this entry: continuing would
				// post its later updates out of order, and would open a second
				// thread if the failed post was the one meant to open the first.
				break
			}

			// Unconditional rather than only when the thread was just opened:
			// the response's channel_id is the thread the message landed in
			// either way, so this also repairs the stored id after a thread had
			// to be reopened.
			state.setThreadID(entry.ID, message.threadID())
			state.record(entry, update.ID, update.at())
			posted++
		}
	}

	return posted, joinErrors(problems)
}

func (a *app) postUpdate(ctx context.Context, webhookURL string, current settings, entry statusEntry, update statusUpdate, threadID string) (webhookMessage, error) {
	if threadID == "" {
		return a.postWebhook(ctx, webhookURL, "", buildPayload(entry, update, true, current.mentionRoleID))
	}

	message, err := a.postWebhook(ctx, webhookURL, threadID, buildPayload(entry, update, false, current.mentionRoleID))
	if err == nil {
		return message, nil
	}

	// A thread deleted in Discord answers 404 for good, which would wedge this
	// entry forever. Opening a replacement loses the updates that were in the
	// old thread but keeps the incident reporting, which is the more useful
	// failure mode.
	var failure *discordError
	if !errors.As(err, &failure) || failure.StatusCode != http.StatusNotFound {
		return webhookMessage{}, err
	}
	log.Printf("thread %s for entry %s is gone; opening a replacement", threadID, entry.ID)

	return a.postWebhook(ctx, webhookURL, "", buildPayload(entry, update, true, current.mentionRoleID))
}

func (a *app) parameterString(ctx context.Context, parameterName string) (string, error) {
	withDecryption := true
	out, err := a.parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: &parameterName, WithDecryption: &withDecryption})
	if err != nil {
		return "", err
	}
	if out.Parameter != nil && out.Parameter.Value != nil {
		return *out.Parameter.Value, nil
	}
	return "", errors.New("parameter has no value")
}

func loadSettings() (settings, error) {
	webhookParameterName, err := requiredEnv("DISCORD_WEBHOOK_PARAMETER_NAME")
	if err != nil {
		return settings{}, err
	}
	stateBucket, err := requiredEnv("STATE_BUCKET")
	if err != nil {
		return settings{}, err
	}
	maxUpdateAge, err := durationEnv("MAX_UPDATE_AGE", defaultMaxUpdateAge)
	if err != nil {
		return settings{}, err
	}
	stateRetention, err := durationEnv("STATE_RETENTION", defaultStateRetention)
	if err != nil {
		return settings{}, err
	}

	return settings{
		baseURL:              envOr("STATUS_PAGE_BASE_URL", defaultStatusPageBaseURL),
		webhookParameterName: webhookParameterName,
		stateBucket:          stateBucket,
		stateKey:             envOr("STATE_KEY", defaultStateKey),
		mentionRoleID:        strings.TrimSpace(os.Getenv("MENTION_ROLE_ID")),
		maxUpdateAge:         maxUpdateAge,
		stateRetention:       stateRetention,
	}, nil
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return parsed, nil
}

func joinErrors(problems []error) error {
	filtered := make([]error, 0, len(problems))
	for _, problem := range problems {
		if problem != nil {
			filtered = append(filtered, problem)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return errors.Join(filtered...)
}
