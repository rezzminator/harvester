// Package clock provides the time operations used by the harvester and its fixtures.
package clock

import (
	"context"
	"time"
)

// Clock is the seam every timing door crosses instead of the bare
// time package: Now for a timestamp, Sleep for a bounded, cancellable wait,
// After/NewTimer for the standard library's channel-based
// primitives.
type Clock interface {
	// Now reports the current time.
	Now() time.Time
	// Sleep blocks for d, or until ctx is done — whichever comes first —
	// returning ctx.Err() only in the latter case. Unlike time.Sleep, a
	// cancelled caller is never left blocked past its own deadline.
	Sleep(ctx context.Context, d time.Duration) error
	// After is time.After: a channel that receives once, d after this call.
	After(d time.Duration) <-chan time.Time
	// NewTimer is time.NewTimer, returned through the Timer seam.
	NewTimer(d time.Duration) Timer
}

// Timer is time.Timer's seam: a channel plus Stop, so a caller holding
// a Clock never touches *time.Timer directly.
type Timer interface {
	// C is the channel the timer fires on — a method, not a field, because
	// an interface cannot expose time.Timer's bare C field.
	C() <-chan time.Time
	Stop() bool
}
