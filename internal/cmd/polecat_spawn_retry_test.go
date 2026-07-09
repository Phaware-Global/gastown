package cmd

import (
	"errors"
	"testing"
)

// TestRetrySessionPane covers the resilient pane-acquisition that prevents a
// transient tmux query failure under load from killing a healthy freshly-spawned
// session (hq-wisp-6dawbp).
func TestRetrySessionPane(t *testing.T) {
	noBackoff := func(int) {}
	alive := func(string) (bool, error) { return true, nil }
	gone := func(string) (bool, error) { return false, nil }

	t.Run("succeeds first try, no retry", func(t *testing.T) {
		calls := 0
		getPane := func(string) (string, error) { calls++; return "%1", nil }
		pane, err := retrySessionPane("s", 5, getPane, alive, noBackoff)
		if err != nil || pane != "%1" {
			t.Fatalf("got (%q, %v), want (%%1, nil)", pane, err)
		}
		if calls != 1 {
			t.Errorf("getPane called %d times, want 1", calls)
		}
	})

	t.Run("flaky query succeeds on retry while session is alive", func(t *testing.T) {
		calls := 0
		getPane := func(string) (string, error) {
			calls++
			if calls < 3 {
				return "", errors.New("tmux busy")
			}
			return "%2", nil
		}
		pane, err := retrySessionPane("s", 5, getPane, alive, noBackoff)
		if err != nil || pane != "%2" {
			t.Fatalf("got (%q, %v), want (%%2, nil)", pane, err)
		}
		if calls != 3 {
			t.Errorf("getPane called %d times, want 3", calls)
		}
	})

	t.Run("alive but query never succeeds returns error after all attempts", func(t *testing.T) {
		calls := 0
		getPane := func(string) (string, error) { calls++; return "", errors.New("tmux busy") }
		if _, err := retrySessionPane("s", 5, getPane, alive, noBackoff); err == nil {
			t.Fatal("want error when the pane query never succeeds")
		}
		if calls != 5 {
			t.Errorf("getPane called %d times, want 5 (full retries while alive)", calls)
		}
	})

	t.Run("confirmed-gone session bails out fast", func(t *testing.T) {
		calls := 0
		getPane := func(string) (string, error) { calls++; return "", errors.New("no session") }
		if _, err := retrySessionPane("s", 5, getPane, gone, noBackoff); err == nil {
			t.Fatal("want error when the session is gone")
		}
		if calls != 1 {
			t.Errorf("getPane called %d times, want 1 (early bail when confirmed gone)", calls)
		}
	})
}
