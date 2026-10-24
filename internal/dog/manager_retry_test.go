package dog

import (
	"errors"
	"testing"
	"time"
)

func workingDogState(name string) *DogState {
	now := time.Now()
	return &DogState{
		Name: name, State: StateWorking, Work: "plugin:x",
		WorkStartedAt: now.Add(-time.Hour), LastActive: now, CreatedAt: now, UpdatedAt: now,
	}
}

func TestClearWorkWithRetry_TransientFailureThenSuccess(t *testing.T) {
	m, _ := testManager(t)
	setupDogWithState(t, m, "alpha", workingDogState("alpha"))

	realWrite := m.writeState
	calls := 0
	m.writeState = func(path string, v interface{}) error {
		calls++
		if calls <= 2 {
			return errors.New("transient: disk busy")
		}
		return realWrite(path, v)
	}

	if err := m.ClearWorkWithRetry("alpha", 3, time.Millisecond); err != nil {
		t.Fatalf("ClearWorkWithRetry = %v, want nil after transient failures", err)
	}
	if calls != 3 {
		t.Errorf("write attempts = %d, want 3", calls)
	}
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateIdle || d.Work != "" || !d.WorkStartedAt.IsZero() {
		t.Errorf("dog = state %q work %q started %v, want idle/empty/zero", d.State, d.Work, d.WorkStartedAt)
	}
}

func TestClearWorkWithRetry_PermanentFailureSurfacesError(t *testing.T) {
	m, _ := testManager(t)
	setupDogWithState(t, m, "alpha", workingDogState("alpha"))

	calls := 0
	m.writeState = func(string, interface{}) error {
		calls++
		return errors.New("permanent: read-only filesystem")
	}

	err := m.ClearWorkWithRetry("alpha", 3, time.Millisecond)
	if err == nil {
		t.Fatal("ClearWorkWithRetry = nil, want error when every write fails")
	}
	if calls != 3 {
		t.Errorf("write attempts = %d, want exactly 3 (bounded)", calls)
	}
	d, gerr := m.Get("alpha")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if d.State != StateWorking {
		t.Errorf("state = %q after failed clear, want working (nothing written)", d.State)
	}
}

func TestClearWorkWithRetry_DoesNotRetryNonTransient(t *testing.T) {
	m, _ := testManager(t)
	calls := 0
	m.writeState = func(string, interface{}) error {
		calls++
		return nil
	}

	// A huge backoff makes a wrongful retry visible as elapsed time.
	start := time.Now()
	if err := m.ClearWorkWithRetry("ghost", 3, 5*time.Second); !errors.Is(err, ErrDogNotFound) {
		t.Fatalf("err = %v, want ErrDogNotFound", err)
	}
	if err := m.ClearWorkWithRetry("../escape", 3, 5*time.Second); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("err = %v, want ErrInvalidName", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v: non-transient errors must not be retried", elapsed)
	}
	if calls != 0 {
		t.Errorf("writes = %d, want 0", calls)
	}
}

func TestWithIdleLock_RunsOnlyWhileIdle(t *testing.T) {
	m, _ := testManager(t)
	setupDogWithState(t, m, "alpha", workingDogState("alpha"))

	ran, err := m.WithIdleLock("alpha", func() error { t.Error("fn ran for a working dog"); return nil })
	if err != nil || ran {
		t.Fatalf("working dog: WithIdleLock = (%v, %v), want (false, nil)", ran, err)
	}

	if err := m.ClearWork("alpha"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ran, err = m.WithIdleLock("alpha", func() error { calls++; return nil })
	if err != nil || !ran || calls != 1 {
		t.Fatalf("idle dog: WithIdleLock = (%v, %v) calls=%d, want (true, nil) and one call", ran, err, calls)
	}
}

func TestWithIdleLock_PropagatesFnError(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	setupDogWithState(t, m, "alpha", &DogState{Name: "alpha", State: StateIdle, LastActive: now, CreatedAt: now, UpdatedAt: now})

	boom := errors.New("kill failed")
	ran, err := m.WithIdleLock("alpha", func() error { return boom })
	if !ran || !errors.Is(err, boom) {
		t.Fatalf("WithIdleLock = (%v, %v), want (true, boom)", ran, err)
	}
}

// The dog's lock is held across fn, so a dispatch cannot assign work between
// the idle check and the kill.
func TestWithIdleLock_BlocksAssignWorkWhileRunning(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	setupDogWithState(t, m, "alpha", &DogState{Name: "alpha", State: StateIdle, LastActive: now, CreatedAt: now, UpdatedAt: now})

	assigned := make(chan struct{})
	_, err := m.WithIdleLock("alpha", func() error {
		go func() {
			_ = m.AssignWork("alpha", "plugin:y")
			close(assigned)
		}()
		select {
		case <-assigned:
			t.Error("AssignWork completed while the idle lock was held")
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-assigned:
	case <-time.After(5 * time.Second):
		t.Fatal("AssignWork never completed after the lock was released")
	}
}
