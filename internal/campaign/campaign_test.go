package campaign

import (
	"fmt"
	"testing"
	"time"

	"github.com/aprilox/blastsmtp/internal/recipients"
)

func makeRecipients(n int) []recipients.Recipient {
	out := make([]recipients.Recipient, n)
	for i := range out {
		email := fmt.Sprintf("dest%d@exemple.fr", i)
		out[i] = recipients.Recipient{
			Email:  email,
			Fields: map[string]string{"email": email, "prenom": fmt.Sprintf("Client%d", i)},
		}
	}
	return out
}

func waitDone(t *testing.T, r *Runner, within time.Duration) *Stats {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !r.Running() {
			return r.Snapshot()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the campaign was still running after %s (state %s)", within, r.Snapshot().State)
	return nil
}

func TestDryRunProcessesEveryRecipient(t *testing.T) {
	r := New()
	err := r.Start(Options{
		Subject:    "Commande n°{{index:1000}}",
		HTML:       "<p>Bonjour {{prenom}}</p>",
		Recipients: makeRecipients(25),
		Workers:    4,
		DryRun:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := waitDone(t, r, 10*time.Second)
	if s.State != StateDone {
		t.Errorf("state = %s, want done", s.State)
	}
	if s.Sent != 25 || s.Failed != 0 || s.Pending != 0 {
		t.Errorf("sent=%d failed=%d pending=%d, want 25/0/0", s.Sent, s.Failed, s.Pending)
	}
	if s.Progress < 99.9 {
		t.Errorf("progress = %v", s.Progress)
	}

	_, rows := r.Report()
	if len(rows) != 25 {
		t.Errorf("report has %d rows, want 25", len(rows))
	}
}

func TestStartRejectsEmptyList(t *testing.T) {
	if err := New().Start(Options{Subject: "x", Text: "y", DryRun: true}); err == nil {
		t.Fatal("Start accepted an empty recipient list")
	}
}

func TestStartRejectsConcurrentRuns(t *testing.T) {
	r := New()
	opts := Options{
		Subject: "x", Text: "y",
		Recipients:    makeRecipients(40),
		Workers:       1,
		RatePerMinute: 60, // slow enough that the run is still going below
		DryRun:        true,
	}
	if err := r.Start(opts); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	if err := r.Start(opts); err != ErrBusy {
		t.Errorf("second Start returned %v, want ErrBusy", err)
	}
}

func TestStopEndsTheRun(t *testing.T) {
	r := New()
	err := r.Start(Options{
		Subject: "x", Text: "y",
		Recipients:    makeRecipients(500),
		Workers:       1,
		RatePerMinute: 120,
		DryRun:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	r.Stop()

	s := waitDone(t, r, 5*time.Second)
	if s.State != StateStopped {
		t.Errorf("state = %s, want stopped", s.State)
	}
	if s.Sent >= 500 {
		t.Errorf("sent = %d: the campaign was not interrupted", s.Sent)
	}
}

func TestPauseThenResume(t *testing.T) {
	r := New()
	err := r.Start(Options{
		Subject: "x", Text: "y",
		Recipients:    makeRecipients(30),
		Workers:       2,
		RatePerMinute: 600,
		DryRun:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Pause()
	if got := r.Snapshot().State; got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
	frozen := r.Snapshot().Sent
	time.Sleep(200 * time.Millisecond)
	if now := r.Snapshot().Sent; now != frozen {
		t.Errorf("a paused campaign kept sending: %d then %d", frozen, now)
	}

	r.Resume()
	if got := r.Snapshot().State; got != StateRunning {
		t.Errorf("state = %s, want running", got)
	}
	if s := waitDone(t, r, 10*time.Second); s.Sent != 30 {
		t.Errorf("sent = %d after resuming, want 30", s.Sent)
	}
}

func TestRateLimitIsHonoured(t *testing.T) {
	r := New()
	// 600 per minute is one every 100ms; 5 messages cannot take less than 400ms.
	start := time.Now()
	err := r.Start(Options{
		Subject: "x", Text: "y",
		Recipients:    makeRecipients(5),
		Workers:       4,
		RatePerMinute: 600,
		DryRun:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, 10*time.Second)

	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Errorf("5 messages at 600/min took %s, the rate limit was not applied", elapsed)
	}
}

func TestSubscribersReceiveEvents(t *testing.T) {
	r := New()
	events, unsubscribe := r.Subscribe()
	defer unsubscribe()

	if err := r.Start(Options{
		Subject: "x", Text: "y",
		Recipients: makeRecipients(3),
		Workers:    1,
		DryRun:     true,
	}); err != nil {
		t.Fatal(err)
	}

	var logs int
	timeout := time.After(5 * time.Second)
	for logs < 3 {
		select {
		case e := <-events:
			if e.Log != nil {
				logs++
			}
		case <-timeout:
			t.Fatalf("only %d log events received, want 3", logs)
		}
	}
}
