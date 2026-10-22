package common

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryPolicyDelay(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 4 * time.Second, Jitter: 0.2}

	if d := p.Delay(1); d != 0 {
		t.Fatalf("first attempt should not wait, got %v", d)
	}
	cases := []struct {
		attempt int
		nominal time.Duration
	}{
		{2, time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 4 * time.Second}, // capped
		{9, 4 * time.Second},
	}
	for _, c := range cases {
		lo := time.Duration(float64(c.nominal) * 0.8)
		for i := 0; i < 50; i++ {
			d := p.Delay(c.attempt)
			if d < lo || d > c.nominal {
				t.Fatalf("attempt %d delay = %v, want within [%v, %v]", c.attempt, d, lo, c.nominal)
			}
		}
	}
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}, func(attempt int) error {
		calls++
		if attempt < 3 {
			return errors.New("invalid connection")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry returned %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryStopsOnSemanticError(t *testing.T) {
	calls := 0
	semantic := errors.New("duplicate key")
	err := Retry(context.Background(), RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond}, func(int) error {
		calls++
		return semantic
	})
	if !errors.Is(err, semantic) {
		t.Fatalf("err = %v, want %v", err, semantic)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry for a non-transient error)", calls)
	}
}

func TestRetryExhaustsAttempts(t *testing.T) {
	calls := 0
	transient := errors.New("server has gone away")
	err := Retry(context.Background(), RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}, func(int) error {
		calls++
		return transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want %v", err, transient)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryHonorsCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	err := Retry(ctx, RetryPolicy{MaxAttempts: 5, BaseDelay: time.Hour}, func(int) error {
		calls++
		cancel() // cancel while the backoff is about to start
		return errors.New("invalid connection")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation did not interrupt the backoff, waited %v", elapsed)
	}
}

func TestRetryZeroAttemptsStillCallsOnce(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), RetryPolicy{MaxAttempts: 0}, func(int) error {
		calls++
		return errors.New("invalid connection")
	})
	if err == nil {
		t.Fatal("expected the transient error to be returned")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
