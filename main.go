package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	// Terraform sets STATUS_PAGE_BASE_URL explicitly for every deployment of
	// this function. This default is only the fallback that keeps the original
	// Claude deployment pointing at the right page during the window between a
	// code push and the apply that sets the variable.
	defaultStatusPageBaseURL = "https://status.claude.com/api/v2"
	defaultStateKey          = "statuspage/state.json"
	defaultMaxUpdateAge      = 24 * time.Hour
	defaultStateRetention    = 30 * 24 * time.Hour
)

type app struct {
	parameters *ssm.Client
	objects    *s3.Client
	httpClient *http.Client
	now        func() time.Time
	// Overridden only by tests. Empty means Discord's real API.
	botAPIBase string
}

type settings struct {
	baseURL string
	// pageLabel names the status page in Discord -- the webhook username and
	// the thread title prefix. Empty is allowed and degrades to an unlabelled
	// "Status", so a deploy that lands before the label is configured keeps
	// working rather than failing every minute.
	pageLabel            string
	pageHost             string
	webhookParameterName string
	stateBucket          string
	stateKey             string
	mentionRoleID        string
	// Optional. Without it the thread title keeps whatever phase marker it was
	// created with, because renaming a thread is the one operation a webhook
	// cannot perform. Everything else works unchanged.
	botTokenParameterName string
	maxUpdateAge          time.Duration
	stateRetention        time.Duration
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

	// Read before the run rather than lazily, so a misconfigured parameter is
	// one log line at the top rather than a failure buried in the first
	// resolution of the day. A missing token is not fatal: titles simply keep
	// the marker they were created with.
	botToken := ""
	if current.botTokenParameterName != "" {
		botToken, err = a.parameterString(ctx, current.botTokenParameterName)
		if err != nil {
			log.Printf("read Discord bot token parameter: %v; thread titles will not be updated", err)
			botToken = ""
		}
	}

	state, err := a.loadState(ctx, current.stateBucket, current.stateKey)
	if err != nil {
		return err
	}

	entries, fetchErr := fetchEntries(ctx, a.httpClient, current.baseURL)

	now := a.now()
	posted, deliverErr := a.deliver(ctx, webhookURL, strings.TrimSpace(botToken), current, entries, state, now)
	state.prune(now, current.stateRetention)

	// Written even when delivery failed part way through, so the updates that
	// did reach Discord are not posted a second time on the next run.
	saveErr := a.saveState(ctx, current.stateBucket, current.stateKey, state)

	log.Printf("checked %d entries, posted %d updates", len(entries), posted)

	return joinErrors([]error{fetchErr, deliverErr, saveErr})
}

// deliver posts every update that has not been posted yet, oldest first, into
// the Discord thread belonging to its Statuspage entry.
func (a *app) deliver(ctx context.Context, webhookURL, botToken string, current settings, entries []statusEntry, state *notifierState, now time.Time) (int, error) {
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
			opening := threadID == ""
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

			if opening {
				// The title was built with this phase when the thread was
				// created, so recording it is enough -- renaming here would
				// spend a rename saying what the title already says.
				state.setTitlePhase(entry.ID, entryPhase(update))
				continue
			}
			a.syncThreadTitle(ctx, botToken, current, entry, update, state)
		}
	}

	return posted, joinErrors(problems)
}

func (a *app) postUpdate(ctx context.Context, webhookURL string, current settings, entry statusEntry, update statusUpdate, threadID string) (webhookMessage, error) {
	if threadID == "" {
		payload := buildPayload(entry, update, true, current)
		message, err := a.postWebhook(ctx, webhookURL, "", payload)
		warnIfMentionDropped(payload, message, err)
		return message, err
	}

	payload := buildPayload(entry, update, false, current)
	message, err := a.postWebhook(ctx, webhookURL, threadID, payload)
	if err == nil {
		warnIfMentionDropped(payload, message, nil)
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

	replacement := buildPayload(entry, update, true, current)
	message, err = a.postWebhook(ctx, webhookURL, "", replacement)
	warnIfMentionDropped(replacement, message, err)

	return message, err
}

// syncThreadTitle keeps the phase marker in the thread's title honest.
//
// It only calls Discord when the phase actually changed, because a rename is
// rate limited to twice per ten minutes for a given thread while messages are
// not -- an incident that walks investigating, identified, monitoring, resolved
// would otherwise spend that whole budget writing the same red circle three
// times before it had a green one to write.
//
// A failure here is logged and dropped rather than returned. The title is a
// convenience for reading the channel list; the update itself has already been
// posted, and failing the run over cosmetics would re-post nothing and hide the
// real state behind an error.
func (a *app) syncThreadTitle(ctx context.Context, botToken string, current settings, entry statusEntry, update statusUpdate, state *notifierState) {
	stored := state.Entries[entry.ID]
	phase := entryPhase(update)
	if botToken == "" || stored.ThreadID == "" || stored.TitlePhase == phase {
		// Still record the phase when there is no token, so that adding one
		// later does not rewrite every existing title at once.
		state.setTitlePhase(entry.ID, phase)
		return
	}

	title := threadTitle(entry, current.pageLabel, phase)
	if err := a.renameThread(ctx, botToken, stored.ThreadID, title); err != nil {
		log.Printf("rename thread %s for entry %s to %q: %v", stored.ThreadID, entry.ID, title, err)
		return
	}
	state.setTitlePhase(entry.ID, phase)
}

// warnIfMentionDropped reports a mention that Discord accepted but did not
// resolve. That is a silent failure worth shouting about: the message still
// posts and still shows the mention text, so the channel looks right while
// nobody is actually notified -- which defeats the reason these updates are
// posted as new messages rather than edits.
//
// The way to land here is to configure MENTION_ROLE_ID with a server id rather
// than a role id. Discord gives the @everyone role the server's own id, so the
// mention renders as "@@everyone" and is ignored, because @everyone can only be
// enabled through allowed_mentions.parse, never through roles.
func warnIfMentionDropped(payload webhookPayload, message webhookMessage, err error) {
	if err != nil {
		return
	}
	for _, dropped := range undeliveredMentions(payload, message) {
		log.Printf("mention of role %s was not delivered: Discord accepted the message but resolved no such role. "+
			"If that id is the server id, it is the @everyone role, which allowed_mentions.roles cannot enable.", dropped)
	}
}

// undeliveredMentions returns the role ids the payload asked to mention that
// Discord did not report back as mentioned.
func undeliveredMentions(payload webhookPayload, message webhookMessage) []string {
	var dropped []string
	for _, wanted := range payload.AllowedMentions.Roles {
		if !slices.Contains(message.MentionRoles, wanted) {
			dropped = append(dropped, wanted)
		}
	}
	return dropped
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

	baseURL := envOr("STATUS_PAGE_BASE_URL", defaultStatusPageBaseURL)

	return settings{
		baseURL:               baseURL,
		pageLabel:             strings.TrimSpace(os.Getenv("PAGE_LABEL")),
		pageHost:              pageHost(baseURL),
		webhookParameterName:  webhookParameterName,
		stateBucket:           stateBucket,
		stateKey:              envOr("STATE_KEY", defaultStateKey),
		mentionRoleID:         strings.TrimSpace(os.Getenv("MENTION_ROLE_ID")),
		botTokenParameterName: strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN_PARAMETER_NAME")),
		maxUpdateAge:          maxUpdateAge,
		stateRetention:        stateRetention,
	}, nil
}

// pageHost is what the embed footer shows, so it is the page a reader would
// visit -- status.claude.com, githubstatus.com -- not the API path underneath
// it. A "www." prefix is dropped because it says nothing.
func pageHost(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(baseURL)
	}
	return strings.TrimPrefix(parsed.Host, "www.")
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
