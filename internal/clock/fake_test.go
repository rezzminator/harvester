package clock

import (
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFakeAdvancePartwayLeavesTheLaterWaiterPending(t *testing.T) {
	fake := NewFake(epoch)
	early := fake.After(5 * time.Millisecond)
	late := fake.After(50 * time.Millisecond)

	fake.Advance(5 * time.Millisecond)

	select {
	case <-early:
	default:
		t.Fatal("early After did not fire by Advance(5ms)")
	}
	select {
	case v := <-late:
		t.Fatalf("late After fired early with %v, want still pending", v)
	default:
	}
	if got := fake.Pending(); got != 1 {
		t.Fatalf("Pending() = %d, want 1 (the still-due late waiter)", got)
	}

	fake.Advance(45 * time.Millisecond)
	select {
	case <-late:
	default:
		t.Fatal("late After did not fire after the second Advance reached it")
	}
	if got := fake.Pending(); got != 0 {
		t.Fatalf("Pending() = %d, want 0 after both waiters fired", got)
	}
}
