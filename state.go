package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const stateVersion = 1

// notifierState is what makes a poller idempotent: which Discord thread belongs
// to which Statuspage entry, and which of that entry's updates have already
// been posted into it.
type notifierState struct {
	Version int                   `json:"version"`
	Entries map[string]entryState `json:"entries"`
}

type entryState struct {
	ThreadID      string    `json:"thread_id"`
	Name          string    `json:"name"`
	PostedUpdates []string  `json:"posted_update_ids"`
	LastPostedAt  time.Time `json:"last_posted_at"`
	// The phase the thread's title currently shows. Stored so the title is only
	// rewritten when it actually changes: renaming a thread costs a Discord API
	// call against a tight rate limit, and doing it on every update would spend
	// that budget saying the same thing.
	TitlePhase string `json:"title_phase,omitempty"`
}

func newState() *notifierState {
	return &notifierState{Version: stateVersion, Entries: map[string]entryState{}}
}

func (s *notifierState) hasPosted(entryID, updateID string) bool {
	for _, posted := range s.Entries[entryID].PostedUpdates {
		if posted == updateID {
			return true
		}
	}
	return false
}

func (s *notifierState) record(entry statusEntry, updateID string, at time.Time) {
	stored := s.Entries[entry.ID]
	stored.Name = entry.Name
	if !s.hasPosted(entry.ID, updateID) {
		stored.PostedUpdates = append(stored.PostedUpdates, updateID)
	}
	if at.After(stored.LastPostedAt) {
		stored.LastPostedAt = at
	}
	s.Entries[entry.ID] = stored
}

func (s *notifierState) setThreadID(entryID, threadID string) {
	stored := s.Entries[entryID]
	stored.ThreadID = threadID
	s.Entries[entryID] = stored
}

func (s *notifierState) setTitlePhase(entryID, phase string) {
	stored := s.Entries[entryID]
	stored.TitlePhase = phase
	s.Entries[entryID] = stored
}

// prune drops entries nothing has been posted to for a long time. Without it
// the object grows forever, and a Statuspage entry that old can no longer
// receive an update that would need its thread back.
func (s *notifierState) prune(now time.Time, retention time.Duration) {
	cutoff := now.Add(-retention)
	for id, stored := range s.Entries {
		if stored.LastPostedAt.Before(cutoff) {
			delete(s.Entries, id)
		}
	}
}

func (a *app) loadState(ctx context.Context, bucket, key string) (*notifierState, error) {
	out, err := a.objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// The first ever run has no object. The Lambda's policy grants
		// s3:ListBucket precisely so this arrives as NoSuchKey rather than as
		// AccessDenied, which would be indistinguishable from a real
		// permissions failure and would make every run look like a cold start.
		var noSuchKey *s3types.NoSuchKey
		var notFound *s3types.NotFound
		if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
			return newState(), nil
		}
		return nil, fmt.Errorf("read state object: %w", err)
	}
	defer out.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(out.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read state object body: %w", err)
	}

	state := newState()
	if len(bytes.TrimSpace(raw)) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(raw, state); err != nil {
		return nil, fmt.Errorf("decode state object: %w", err)
	}
	if state.Entries == nil {
		state.Entries = map[string]entryState{}
	}
	state.Version = stateVersion

	return state, nil
}

func (a *app) saveState(ctx context.Context, bucket, key string, state *notifierState) error {
	// Sorted keys keep the stored object stable between runs that changed
	// nothing, which makes its version history readable.
	ids := make([]string, 0, len(state.Entries))
	for id := range state.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make(map[string]entryState, len(ids))
	for _, id := range ids {
		ordered[id] = state.Entries[id]
	}

	body, err := json.MarshalIndent(&notifierState{Version: state.Version, Entries: ordered}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	if _, err := a.objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("write state object: %w", err)
	}

	return nil
}
