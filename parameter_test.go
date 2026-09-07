package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fakeParameters stands in for SSM. It counts calls per parameter name, which
// is what the cache is there to keep down.
type fakeParameters struct {
	values map[string]string
	calls  map[string]int
	err    error
}

func newFakeParameters(values map[string]string) *fakeParameters {
	return &fakeParameters{values: values, calls: map[string]int{}}
}

func (f *fakeParameters) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.calls[*in.Name]++
	if f.err != nil {
		return nil, f.err
	}
	value, ok := f.values[*in.Name]
	if !ok {
		return nil, errors.New("parameter not found")
	}
	return &ssm.GetParameterOutput{Parameter: &types.Parameter{Value: &value}}, nil
}

func readParameter(t *testing.T, instance *app, name string) string {
	t.Helper()

	value, err := instance.parameterString(context.Background(), name)
	if err != nil {
		t.Fatalf("read parameter %s: %v", name, err)
	}
	return value
}

func TestParameterStringReadsOnceWithinTTL(t *testing.T) {
	fake := newFakeParameters(map[string]string{"/webhook": "first"})
	now := time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)
	instance := &app{parameters: fake, now: func() time.Time { return now }}

	if got := readParameter(t, instance, "/webhook"); got != "first" {
		t.Fatalf("first read = %q, want %q", got, "first")
	}

	// A changed value proves the second read came from the cache rather than
	// from SSM, on top of the call count.
	fake.values["/webhook"] = "second"
	now = now.Add(parameterCacheTTL - time.Second)

	if got := readParameter(t, instance, "/webhook"); got != "first" {
		t.Fatalf("cached read = %q, want %q", got, "first")
	}
	if fake.calls["/webhook"] != 1 {
		t.Fatalf("read SSM %d times, want 1", fake.calls["/webhook"])
	}
}

func TestParameterStringRereadsAfterTTL(t *testing.T) {
	fake := newFakeParameters(map[string]string{"/webhook": "first"})
	now := time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)
	instance := &app{parameters: fake, now: func() time.Time { return now }}

	readParameter(t, instance, "/webhook")

	fake.values["/webhook"] = "second"
	now = now.Add(parameterCacheTTL)

	if got := readParameter(t, instance, "/webhook"); got != "second" {
		t.Fatalf("read after expiry = %q, want %q", got, "second")
	}
	if fake.calls["/webhook"] != 2 {
		t.Fatalf("read SSM %d times, want 2", fake.calls["/webhook"])
	}
}

func TestParameterStringCachesEachNameSeparately(t *testing.T) {
	fake := newFakeParameters(map[string]string{"/webhook": "hook", "/token": "token"})
	instance := &app{parameters: fake, now: time.Now}

	if got := readParameter(t, instance, "/webhook"); got != "hook" {
		t.Fatalf("webhook = %q, want %q", got, "hook")
	}
	if got := readParameter(t, instance, "/token"); got != "token" {
		t.Fatalf("token = %q, want %q", got, "token")
	}
	if got := readParameter(t, instance, "/webhook"); got != "hook" {
		t.Fatalf("second webhook read = %q, want %q", got, "hook")
	}

	if fake.calls["/webhook"] != 1 || fake.calls["/token"] != 1 {
		t.Fatalf("read SSM %d/%d times, want 1/1", fake.calls["/webhook"], fake.calls["/token"])
	}
}

// A failed read must not be remembered: the next run should try SSM again
// rather than serve nothing for an hour.
func TestParameterStringDoesNotCacheFailures(t *testing.T) {
	fake := newFakeParameters(map[string]string{"/webhook": "hook"})
	fake.err = errors.New("access denied")
	instance := &app{parameters: fake, now: time.Now}

	if _, err := instance.parameterString(context.Background(), "/webhook"); err == nil {
		t.Fatal("failed read returned no error")
	}

	fake.err = nil
	if got := readParameter(t, instance, "/webhook"); got != "hook" {
		t.Fatalf("read after failure = %q, want %q", got, "hook")
	}
	if fake.calls["/webhook"] != 2 {
		t.Fatalf("read SSM %d times, want 2", fake.calls["/webhook"])
	}
}
