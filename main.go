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
	"sync"
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
	sourceStatuspage         = "statuspage"
	sourceStatusIQ           = "statusiq"
	defaultStateKey          = "statuspage/state.json"
	defaultMaxUpdateAge      = 24 * time.Hour
	defaultStateRetention    = 30 * 24 * time.Hour

	// parameterCacheTTL bounds how stale a cached parameter may be. Every run
	// reads the webhook parameter, and each read decrypts a SecureString; at
	// one run a minute across nine deployments those decryptions were the
	// whole KMS bill. Lambda reuses the execution environment between runs, so
	// caching here removes almost all of them, and an hour is well inside the
	// time it takes to roll a webhook out anyway.
	parameterCacheTTL = time.Hour
)

// parameterReader is the slice of the SSM API this function uses. Narrowed to
// an interface so the cache can be exercised without an AWS client.
type parameterReader interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// cachedParameter is a parameter value together with the time it was read, so
// parameterString can tell whether it has outlived parameterCacheTTL.
type cachedParameter struct {
	value  string
	readAt time.Time
}

type app struct {
	parameters parameterReader
	objects    *s3.Client
	httpClient *http.Client
	now        func() time.Time
	// Overridden only by tests. Empty means Discord's real API.
	botAPIBase string

	// Survives between invocations, because Lambda reuses the execution
	// environment. Guarded because nothing promises that reuse is
	// single-threaded.
	parameterMutex sync.Mutex
	parameterCache map[string]cachedParameter
}

type settings struct {
	// source picks the parser: "statuspage" for Atlassian Statuspage's JSON
	// API, "statusiq" for the HTML of a StatusIQ (Site24x7) page.
	source  string
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
	// Whether the page's scheduled-maintenance feed is worth reading. Off for
	// pages that post a window per datacenter, where it drowns the incidents.
	includeMaintenance bool
	// Optional. Without it a resolved post is never closed, because closing is
	// the one operation a webhook cannot perform. Everything else works
	// unchanged.
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
	// resolution of the day. A missing token is not fatal: posts simply stay
	// open.
	botToken := ""
	if current.botTokenParameterName != "" {
		botToken, err = a.parameterString(ctx, current.botTokenParameterName)
		if err != nil {
			log.Printf("read Discord bot token parameter: %v; resolved posts will not be closed", err)
			botToken = ""
		}
	}

	state, err := a.loadState(ctx, current.stateBucket, current.stateKey)
	if err != nil {
		return err
	}

	var entries []statusEntry
	var fetchErr error
	switch current.source {
	case sourceStatusIQ:
		entries, fetchErr = fetchStatusIQEntries(ctx, a.httpClient, current.baseURL, current.includeMaintenance)
	default:
		entries, fetchErr = fetchEntries(ctx, a.httpClient, current.baseURL, current.includeMaintenance)
	}

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
			entry.Mentioned = state.Entries[entry.ID].Mentioned
			message, mentioned, err := a.postUpdate(ctx, webhookURL, current, entry, update, threadID)
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
			if mentioned {
				state.setMentioned(entry.ID)
			}
			posted++

			a.syncThreadClosed(ctx, botToken, entry, update, state)
		}
	}

	return posted, joinErrors(problems)
}

// postUpdate reports, alongside the message, whether the payload asked for the
// role mention, so the caller can remember that the entry has pinged.
func (a *app) postUpdate(ctx context.Context, webhookURL string, current settings, entry statusEntry, update statusUpdate, threadID string) (webhookMessage, bool, error) {
	if threadID == "" {
		payload := buildPayload(entry, update, true, current)
		message, err := a.postWebhook(ctx, webhookURL, "", payload)
		warnIfMentionDropped(payload, message, err)
		return message, len(payload.AllowedMentions.Roles) > 0, err
	}

	payload := buildPayload(entry, update, false, current)
	message, err := a.postWebhook(ctx, webhookURL, threadID, payload)
	if err == nil {
		warnIfMentionDropped(payload, message, nil)
		return message, len(payload.AllowedMentions.Roles) > 0, nil
	}

	// A thread deleted in Discord answers 404 for good, which would wedge this
	// entry forever. Opening a replacement loses the updates that were in the
	// old thread but keeps the incident reporting, which is the more useful
	// failure mode.
	var failure *discordError
	if !errors.As(err, &failure) || failure.StatusCode != http.StatusNotFound {
		return webhookMessage{}, false, err
	}
	log.Printf("thread %s for entry %s is gone; opening a replacement", threadID, entry.ID)

	replacement := buildPayload(entry, update, true, current)
	message, err = a.postWebhook(ctx, webhookURL, "", replacement)
	warnIfMentionDropped(replacement, message, err)

	return message, len(replacement.AllowedMentions.Roles) > 0, err
}

// syncThreadClosed closes a forum post once its incident resolves, and notes
// when Discord has reopened one.
//
// Closing is what marks an incident done here, rather than a marker in the
// title: an open post means something is still happening, which is the question
// the forum's post list exists to answer.
//
// Posting to an archived thread reopens it, so a postmortem arriving after the
// resolution lands in the post and closes it again on the next terminal update.
// That is also why the reopen is recorded rather than assumed: without it the
// post would stay open, because the close call is only made on the transition.
//
// A failure here is logged and dropped rather than returned. The update itself
// has already been posted, and failing the run over the post's open/closed state
// would deliver nothing new while hiding the real state behind an error.
func (a *app) syncThreadClosed(ctx context.Context, botToken string, entry statusEntry, update statusUpdate, state *notifierState) {
	stored := state.Entries[entry.ID]
	if stored.ThreadID == "" {
		return
	}

	if !isTerminal(update.Status) {
		// This update reopened the post by being posted into it.
		if stored.ThreadClosed {
			state.setThreadClosed(entry.ID, false)
		}
		return
	}

	if botToken == "" {
		return
	}
	if err := a.closeThread(ctx, botToken, stored.ThreadID); err != nil {
		log.Printf("close thread %s for entry %s: %v", stored.ThreadID, entry.ID, err)
		return
	}
	state.setThreadClosed(entry.ID, true)
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
	if value, ok := a.cachedParameterValue(parameterName); ok {
		return value, nil
	}

	withDecryption := true
	out, err := a.parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: &parameterName, WithDecryption: &withDecryption})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", errors.New("parameter has no value")
	}

	a.cacheParameterValue(parameterName, *out.Parameter.Value)
	return *out.Parameter.Value, nil
}

// cachedParameterValue returns the cached value of parameterName while it is
// younger than parameterCacheTTL. Only successful reads are cached: a failure
// should be retried on the next run rather than remembered for an hour.
func (a *app) cachedParameterValue(parameterName string) (string, bool) {
	a.parameterMutex.Lock()
	defer a.parameterMutex.Unlock()

	entry, ok := a.parameterCache[parameterName]
	if !ok || a.clock().Sub(entry.readAt) >= parameterCacheTTL {
		return "", false
	}
	return entry.value, true
}

func (a *app) cacheParameterValue(parameterName, value string) {
	a.parameterMutex.Lock()
	defer a.parameterMutex.Unlock()

	if a.parameterCache == nil {
		a.parameterCache = map[string]cachedParameter{}
	}
	a.parameterCache[parameterName] = cachedParameter{value: value, readAt: a.clock()}
}

// clock is now, or the real clock for the tests that build an app without one.
func (a *app) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}
	return a.now()
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

	source := strings.ToLower(envOr("STATUS_PAGE_SOURCE", sourceStatuspage))
	if source != sourceStatuspage && source != sourceStatusIQ {
		return settings{}, fmt.Errorf("STATUS_PAGE_SOURCE must be %q or %q, got %q", sourceStatuspage, sourceStatusIQ, source)
	}

	return settings{
		source:                source,
		baseURL:               baseURL,
		pageLabel:             strings.TrimSpace(os.Getenv("PAGE_LABEL")),
		pageHost:              pageHost(baseURL),
		webhookParameterName:  webhookParameterName,
		stateBucket:           stateBucket,
		stateKey:              envOr("STATE_KEY", defaultStateKey),
		mentionRoleID:         strings.TrimSpace(os.Getenv("MENTION_ROLE_ID")),
		includeMaintenance:    !boolEnv("SKIP_SCHEDULED_MAINTENANCE"),
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

func boolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "true", "1", "yes":
		return true
	}
	return false
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
