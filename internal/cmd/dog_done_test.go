package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
)

// fakeDogStore is a dogDoneStore whose state write can be made to fail.
type fakeDogStore struct {
	d        *dog.Dog
	clearErr error
	events   *[]string
}

func (f *fakeDogStore) Get(string) (*dog.Dog, error) { return f.d, nil }

func (f *fakeDogStore) ClearWorkWithRetry(string, int, time.Duration) error {
	*f.events = append(*f.events, "clear")
	return f.clearErr
}

func workingFake(events *[]string, clearErr error) *fakeDogStore {
	return &fakeDogStore{
		d:        &dog.Dog{Name: "delta", State: dog.StateWorking, Work: "plugin:x"},
		clearErr: clearErr,
		events:   events,
	}
}

// The state write must land before any slow mail cleanup, so a process killed
// mid-cleanup (agent tool timeout) cannot leave the dog stuck working.
func TestCompleteDogDone_ClearsStateBeforeMailCleanup(t *testing.T) {
	var events []string
	var out bytes.Buffer

	cleared, err := completeDogDone(&out, workingFake(&events, nil), "delta", func(time.Time) { events = append(events, "mail") })
	if err != nil || !cleared {
		t.Fatalf("completeDogDone = (%v, %v), want (true, nil)", cleared, err)
	}
	if got := strings.Join(events, ","); got != "clear,mail" {
		t.Errorf("order = %q, want clear,mail", got)
	}
	if !strings.Contains(out.String(), "returned to kennel") {
		t.Errorf("output = %q, want success line", out.String())
	}
}

func TestCompleteDogDone_PermanentFailureIsLoudAndClaimsNothing(t *testing.T) {
	var events []string
	var out bytes.Buffer

	cleared, err := completeDogDone(&out, workingFake(&events, errors.New("disk full")), "delta", func(time.Time) { events = append(events, "mail") })
	if cleared {
		t.Error("cleared = true on a failed write")
	}
	if err == nil {
		t.Fatal("completeDogDone = nil, want error so gt dog done exits non-zero")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want the cause surfaced", err)
	}
	if strings.Contains(out.String(), "returned to kennel") || strings.Contains(out.String(), "idle") {
		t.Errorf("output %q claims success", out.String())
	}
	if got := strings.Join(events, ","); got != "clear" {
		t.Errorf("events = %q, want only clear (no cleanup/termination after a failed write)", got)
	}
}

func TestCompleteDogDone_AlreadyIdleIsNoop(t *testing.T) {
	var events []string
	var out bytes.Buffer
	f := workingFake(&events, nil)
	f.d = &dog.Dog{Name: "delta", State: dog.StateIdle}

	cleared, err := completeDogDone(&out, f, "delta", func(time.Time) { events = append(events, "mail") })
	if err != nil || cleared {
		t.Fatalf("completeDogDone = (%v, %v), want (false, nil): nothing to terminate", cleared, err)
	}
	if got := strings.Join(events, ","); got != "mail" {
		t.Errorf("events = %q, want only mail cleanup (plugin mails always closed)", got)
	}
	if !strings.Contains(out.String(), "already idle") {
		t.Errorf("output = %q", out.String())
	}
}

func TestCompleteDogDone_CleanupReceivesClearTime(t *testing.T) {
	var events []string
	var out bytes.Buffer
	var got time.Time

	before := time.Now()
	if _, err := completeDogDone(&out, workingFake(&events, nil), "delta", func(at time.Time) { got = at }); err != nil {
		t.Fatal(err)
	}
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("cleanup cutoff = %v, want the moment the dog went idle (>= %v)", got, before)
	}
}

// fakeMailbox records which messages were archived.
type fakeMailbox struct {
	msgs     []*mail.Message
	archived []string
}

func (f *fakeMailbox) List() ([]*mail.Message, error) { return f.msgs, nil }

func (f *fakeMailbox) Archive(id string) error {
	f.archived = append(f.archived, id)
	return nil
}

func pluginMail(id, subject, from string, at time.Time) *mail.Message {
	return &mail.Message{ID: id, Subject: subject, From: from, To: "deacon/dogs/delta", Timestamp: at}
}

// A dispatch that lands between the clear and the cleanup sends a new job's
// mail after the cutoff; archiving it would strand the dog on a job it never
// receives instructions for.
func TestArchivePluginMails_KeepsMailSentAfterCutoff(t *testing.T) {
	cutoff := time.Now()
	mb := &fakeMailbox{msgs: []*mail.Message{
		pluginMail("old-read", "Plugin: X", "deacon/", cutoff.Add(-30*time.Minute)),
		pluginMail("old-daemon", "Plugin: X", "daemon", cutoff.Add(-time.Minute)),
		pluginMail("new-job", "Plugin: Y", "deacon/", cutoff.Add(2*time.Second)),
		pluginMail("new-daemon-job", "Plugin: Z", "daemon", cutoff.Add(time.Second)),
		pluginMail("human", "Plugin: X notes", "mayor/", cutoff.Add(-time.Hour)),
	}}

	n := archivePluginMails(mb, "delta", cutoff)

	if n != 2 || strings.Join(mb.archived, ",") != "old-read,old-daemon" {
		t.Errorf("archived %v (n=%d), want only the two old deacon/daemon plugin mails", mb.archived, n)
	}
}

// Mail timestamps are stored to whole seconds, so a dispatch sent a moment
// after the cutoff can read as earlier than it. That mail must survive.
func TestArchivePluginMails_KeepsMailStoredInTheCutoffSecond(t *testing.T) {
	cutoff := time.Date(2026, 10, 24, 17, 8, 5, 700_000_000, time.UTC)
	sent := cutoff.Add(150 * time.Millisecond)
	mb := &fakeMailbox{msgs: []*mail.Message{
		pluginMail("old", "Plugin: X", "daemon", cutoff.Add(-2*time.Second).Truncate(time.Second)),
		pluginMail("same-second-job", "Plugin: Y", "daemon", sent.Truncate(time.Second)),
	}}

	n := archivePluginMails(mb, "delta", cutoff)

	if n != 1 || strings.Join(mb.archived, ",") != "old" {
		t.Errorf("archived %v (n=%d), want only the mail from before the cutoff's second", mb.archived, n)
	}
}

func idleDogState(name string) *dog.DogState {
	now := time.Now()
	return &dog.DogState{Name: name, State: dog.StateIdle, LastActive: now, CreatedAt: now, UpdatedAt: now}
}

func TestTerminateDogSession_KillsWhenStillIdle(t *testing.T) {
	m, root := testDogManager(t)
	setupTestDog(t, m, root, "delta", idleDogState("delta"))

	kills, nudges := 0, 0
	killed, err := terminateDogSession(m, "delta", func() error { kills++; return nil }, func() error { nudges++; return nil })
	if err != nil || !killed || kills != 1 || nudges != 0 {
		t.Fatalf("terminateDogSession = (%v, %v) kills=%d nudges=%d, want (true, nil), one kill and no nudge", killed, err, kills, nudges)
	}
}

// A dispatch between the clear and the kill gives the dog a new job and
// session; the kill must leave both alone.
func TestTerminateDogSession_SkipsKillWhenReassigned(t *testing.T) {
	m, root := testDogManager(t)
	setupTestDog(t, m, root, "delta", idleDogState("delta"))
	if err := m.AssignWork("delta", "plugin:y"); err != nil {
		t.Fatal(err)
	}

	kills, nudges := 0, 0
	killed, err := terminateDogSession(m, "delta", func() error { kills++; return nil }, func() error { nudges++; return nil })
	if err != nil || killed || kills != 0 {
		t.Fatalf("terminateDogSession = (%v, %v) kills=%d, want (false, nil) and no kill", killed, err, kills)
	}
	// The reused session's agent sits at its prompt; it must be told to read
	// the new job's mail.
	if nudges != 1 {
		t.Errorf("nudges = %d, want 1 so the reassigned session starts its new job", nudges)
	}
}

// A failed nudge is reported, so gt dog done can warn about it.
func TestTerminateDogSession_ReportsFailedNudge(t *testing.T) {
	m, root := testDogManager(t)
	setupTestDog(t, m, root, "delta", idleDogState("delta"))
	if err := m.AssignWork("delta", "plugin:y"); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("no such session")
	killed, err := terminateDogSession(m, "delta", func() error { return nil }, func() error { return wantErr })
	if killed || !errors.Is(err, wantErr) {
		t.Fatalf("terminateDogSession = (%v, %v), want (false, %v)", killed, err, wantErr)
	}
}
