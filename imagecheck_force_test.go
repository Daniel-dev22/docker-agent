package main

import (
	"testing"
	"time"
)

// TestForceRecheckDebounced verifies the refresh endpoint admits at most one
// TTL-bypassing force per debounce window: the first call sets forceNext, a second
// call inside the window does not re-admit, and a call past the window does.
func TestForceRecheckDebounced(t *testing.T) {
	ic := &imageChecker{
		forceDebounce: 15 * time.Second,
		trigger:       make(chan struct{}, 1),
	}

	// First force is admitted.
	ic.ForceRecheckDebounced()
	if !ic.forceNext {
		t.Fatal("first force should be admitted (forceNext=true)")
	}

	// Simulate the pass consuming the force flag, then refresh again immediately:
	// within the debounce window it must NOT re-admit (no back-to-back forced pass).
	ic.forceNext = false
	ic.ForceRecheckDebounced()
	if ic.forceNext {
		t.Fatal("second force within debounce window should not be admitted")
	}

	// Once the window has elapsed, a force is admitted again.
	ic.lastForce = time.Now().Add(-16 * time.Second)
	ic.ForceRecheckDebounced()
	if !ic.forceNext {
		t.Fatal("force after debounce window should be admitted")
	}
}
