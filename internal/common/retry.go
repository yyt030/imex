package common

import (
	"context"
	"math/rand/v2"
	"time"
)

// RetryPolicy bounds how a transient failure is retried.
//
// MaxAttempts counts the first try, so 3 means "try, retry, retry".
// BaseDelay is the wait after the first failure and doubles per attempt, capped
// at MaxDelay. Jitter is the fraction (0–1) by which a delay may be shortened,
// so concurrent workers that hit the same outage do not reconnect in lockstep.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64
}

// DefaultRetryPolicy is tuned for LB/NAT half-open connections on cloud
// networks: three attempts, roughly 1s then 2s of backoff (capped at 4s).
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   time.Second,
		MaxDelay:    4 * time.Second,
		Jitter:      0.2,
	}
}

// Delay returns the wait before the given attempt (1-based). Attempt 1 is the
// first try and has no delay; attempt 2 waits BaseDelay, then it doubles.
func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	d := p.BaseDelay
	if d <= 0 {
		return 0
	}
	for i := 2; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	if p.Jitter > 0 {
		// Only shorten: the resulting delay stays in [d*(1-jitter), d].
		d -= time.Duration(rand.Float64() * p.Jitter * float64(d))
	}
	return d
}

// Retry runs fn until it succeeds, returns a non-transient error, exhausts
// MaxAttempts, or ctx is done. fn receives the 1-based attempt number so it can
// perform reconnect/replay work before retrying; it is called at least once.
//
// IsTransient decides what may be retried. Context cancellation is checked
// before every attempt and interrupts any pending backoff, returning ctx.Err().
func Retry(ctx context.Context, p RetryPolicy, fn func(attempt int) error) error {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	var err error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = fn(attempt)
		if err == nil {
			return nil
		}
		if !IsTransient(err) || attempt == p.MaxAttempts {
			return err
		}

		delay := p.Delay(attempt + 1)
		Logger.Warn("Transient database error, retrying",
			"attempt", attempt,
			"max_attempts", p.MaxAttempts,
			"delay", delay,
			"error", err,
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}
