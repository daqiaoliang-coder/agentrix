// Package budget limits an agent run by iteration count and wall-clock deadline.
package budget

import (
	"errors"
	"time"
)

var (
	ErrIterationLimit = errors.New("budget: iteration limit exceeded")
	ErrDeadline       = errors.New("budget: deadline exceeded")
)

// Budget tracks how many model iterations a run has consumed and whether its
// deadline has passed. Zero maxIterations/timeout means the corresponding
// limit is disabled.
type Budget struct {
	maxIterations int
	deadline      time.Time

	iteration int
}

func New(maxIterations int, timeout time.Duration) *Budget {
	b := &Budget{maxIterations: maxIterations}
	if timeout > 0 {
		b.deadline = time.Now().Add(timeout)
	}
	return b
}

func (b *Budget) CheckDeadline() error {
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		return ErrDeadline
	}
	return nil
}

// NextIteration advances the iteration counter and returns an error once the
// limit is exceeded.
func (b *Budget) NextIteration() error {
	b.iteration++
	if b.maxIterations > 0 && b.iteration > b.maxIterations {
		return ErrIterationLimit
	}
	return nil
}

func (b *Budget) Iteration() int { return b.iteration }
