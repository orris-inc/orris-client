package tunnel

import (
	"math"
	"math/rand"
	"time"
)

// Backoff implements exponential backoff with jitter.
type Backoff struct {
	// Initial is the initial backoff interval.
	Initial time.Duration
	// Max is the maximum backoff interval.
	Max time.Duration
	// Multiplier is the factor by which the interval increases.
	Multiplier float64
	// Jitter is the randomization factor (0.0 to 1.0).
	// For example, 0.2 means ±20% randomization.
	Jitter float64

	attempt int
}

// DefaultBackoff returns a Backoff with sensible defaults.
func DefaultBackoff() *Backoff {
	return &Backoff{
		Initial:    1 * time.Second,
		Max:        60 * time.Second,
		Multiplier: 2.0,
		Jitter:     0.2,
	}
}

// Next returns the next backoff duration and increments the attempt counter.
func (b *Backoff) Next() time.Duration {
	if b.Initial <= 0 {
		b.Initial = time.Second
	}
	if b.Max <= 0 {
		b.Max = 60 * time.Second
	}
	if b.Multiplier <= 0 {
		b.Multiplier = 2.0
	}

	// Calculate base interval: initial * (multiplier ^ attempt)
	interval := float64(b.Initial) * math.Pow(b.Multiplier, float64(b.attempt))

	// Cap at max interval
	if interval > float64(b.Max) {
		interval = float64(b.Max)
	}

	// Apply jitter: interval * (1 + random(-jitter, +jitter))
	if b.Jitter > 0 {
		jitter := (rand.Float64()*2 - 1) * b.Jitter // range: [-jitter, +jitter]
		interval = interval * (1 + jitter)
	}

	b.attempt++

	return time.Duration(interval)
}

// Reset resets the attempt counter.
func (b *Backoff) Reset() {
	b.attempt = 0
}

// Attempt returns the current attempt number.
func (b *Backoff) Attempt() int {
	return b.attempt
}
