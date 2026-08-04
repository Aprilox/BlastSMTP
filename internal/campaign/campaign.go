// Package campaign runs a rendered mailing across a pool of SMTP workers,
// pacing the sends and streaming progress to any number of subscribers.
package campaign

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aprilox/blastsmtp/internal/mailer"
	"github.com/aprilox/blastsmtp/internal/recipients"
	"github.com/aprilox/blastsmtp/internal/tmpl"
)

// Campaign states, as reported to the UI.
const (
	StateIdle    = "idle"
	StateRunning = "running"
	StatePaused  = "paused"
	StateDone    = "done"
	StateStopped = "stopped"
	StateFailed  = "failed"
)

// Options is the full description of a run.
type Options struct {
	SMTP mailer.Config `json:"smtp"`

	Subject string `json:"subject"`
	HTML    string `json:"html"`
	Text    string `json:"text"`

	Attachments []mailer.Attachment    `json:"-"`
	Recipients  []recipients.Recipient `json:"-"`
	Headers     map[string]string      `json:"headers"`

	// Workers is the number of parallel SMTP connections.
	Workers int `json:"workers"`
	// RatePerMinute caps the global send rate. 0 means no limit.
	RatePerMinute int `json:"ratePerMinute"`
	// BatchSize and BatchPauseSeconds insert a cooldown every N messages,
	// which is what most shared relays expect from a bulk sender.
	BatchSize         int `json:"batchSize"`
	BatchPauseSeconds int `json:"batchPauseSeconds"`
	// MaxRetries is the number of extra attempts after a temporary failure.
	MaxRetries int `json:"maxRetries"`
	// ReconnectEvery forces a fresh SMTP session every N messages per worker.
	ReconnectEvery int `json:"reconnectEvery"`
	// IndexStart is the first value taken by the {{index}} placeholder.
	IndexStart int `json:"indexStart"`
	// Seed makes the random placeholders reproducible between preview and send.
	Seed int64 `json:"seed"`
	// StopOnError aborts the whole campaign on the first permanent failure.
	StopOnError bool `json:"stopOnError"`
	// DryRun renders and validates every message without opening any
	// connection. Nothing leaves the machine.
	DryRun bool `json:"dryRun"`
}

func (o *Options) applyDefaults() {
	if o.Workers <= 0 {
		o.Workers = 1
	}
	if o.Workers > 32 {
		o.Workers = 32
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.IndexStart == 0 {
		o.IndexStart = 1
	}
	if o.Seed == 0 {
		o.Seed = time.Now().UnixNano()
	}
	if o.ReconnectEvery < 0 {
		o.ReconnectEvery = 0
	}
}

// Stats is the live counter set displayed by the dashboard.
type Stats struct {
	State      string  `json:"state"`
	Total      int     `json:"total"`
	Sent       int     `json:"sent"`
	Failed     int     `json:"failed"`
	Retries    int     `json:"retries"`
	Pending    int     `json:"pending"`
	Progress   float64 `json:"progress"`
	ElapsedMs  int64   `json:"elapsedMs"`
	RatePerMin float64 `json:"ratePerMin"`
	ETASeconds int     `json:"etaSeconds"`
	StartedAt  string  `json:"startedAt"`
	Error      string  `json:"error"`
	DryRun     bool    `json:"dryRun"`
}

// LogEntry is one line of the delivery journal.
type LogEntry struct {
	Index      int    `json:"index"`
	Email      string `json:"email"`
	Status     string `json:"status"` // sent | failed | retry
	Message    string `json:"message"`
	Code       int    `json:"code"`
	Attempt    int    `json:"attempt"`
	Worker     int    `json:"worker"`
	DurationMs int64  `json:"durationMs"`
	At         string `json:"at"`
}

// Event is what subscribers receive over the SSE stream.
type Event struct {
	Type  string    `json:"type"` // stats | log | state | notice
	Stats *Stats    `json:"stats,omitempty"`
	Log   *LogEntry `json:"log,omitempty"`
	Text  string    `json:"text,omitempty"`
}

// Runner owns at most one campaign at a time.
type Runner struct {
	mu      sync.Mutex
	opts    Options
	stats   Stats
	logs    []LogEntry
	started time.Time
	cancel  context.CancelFunc
	running bool

	gate *gate
	subs map[int]chan Event
	next int
}

// New returns an idle runner.
func New() *Runner {
	return &Runner{
		stats: Stats{State: StateIdle},
		subs:  map[int]chan Event{},
	}
}

// ErrBusy is returned when a campaign is already in flight.
var ErrBusy = errors.New("a campaign is already running")

// Start validates the options and launches the run in the background.
func (r *Runner) Start(opts Options) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return ErrBusy
	}
	if len(opts.Recipients) == 0 {
		r.mu.Unlock()
		return errors.New("the recipient list is empty")
	}
	if !opts.DryRun {
		if err := opts.SMTP.Validate(); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	opts.applyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	r.opts = opts
	r.cancel = cancel
	r.running = true
	r.started = time.Now()
	r.logs = make([]LogEntry, 0, len(opts.Recipients))
	r.gate = newGate()
	r.stats = Stats{
		State:     StateRunning,
		Total:     len(opts.Recipients),
		Pending:   len(opts.Recipients),
		StartedAt: r.started.Format(time.RFC3339),
		DryRun:    opts.DryRun,
	}
	r.mu.Unlock()

	r.broadcast(Event{Type: "state", Stats: r.Snapshot()})
	go r.run(ctx, opts)
	return nil
}

func (r *Runner) run(ctx context.Context, opts Options) {
	defer r.cancel()

	jobs := make(chan job)
	pace := newPacer(opts)

	var wg sync.WaitGroup
	for w := 1; w <= opts.Workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			r.work(ctx, worker, opts, jobs, pace)
		}(w)
	}

	// A ticker keeps elapsed time, rate and ETA moving even while a worker is
	// blocked on a slow relay.
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.broadcast(Event{Type: "stats", Stats: r.Snapshot()})
			case <-done:
				return
			}
		}
	}()

feed:
	for i, rcpt := range opts.Recipients {
		select {
		case jobs <- job{index: i + 1, recipient: rcpt}:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(done)

	r.mu.Lock()
	r.running = false
	switch {
	case r.stats.State == StateFailed:
		// keep the failure state and its message
	case ctx.Err() != nil:
		r.stats.State = StateStopped
	default:
		r.stats.State = StateDone
	}
	r.stats.Pending = r.stats.Total - r.stats.Sent - r.stats.Failed
	r.mu.Unlock()

	final := r.Snapshot()
	r.broadcast(Event{Type: "state", Stats: final})
}

type job struct {
	index     int
	recipient recipients.Recipient
}

func (r *Runner) work(ctx context.Context, worker int, opts Options, jobs <-chan job, pace *pacer) {
	var conn *mailer.Conn
	sentOnConn := 0
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()

	for j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if err := r.gate.wait(ctx); err != nil {
			return
		}
		if err := pace.wait(ctx); err != nil {
			return
		}

		msg, err := r.render(opts, j)
		if err != nil {
			r.record(LogEntry{
				Index: j.index, Email: j.recipient.Email, Status: "failed",
				Message: err.Error(), Worker: worker, Attempt: 1,
			})
			continue
		}

		if opts.DryRun {
			// Still build the MIME payload so a malformed message is caught.
			start := time.Now()
			if _, err := msg.Build(); err != nil {
				r.record(LogEntry{Index: j.index, Email: j.recipient.Email, Status: "failed",
					Message: err.Error(), Worker: worker, Attempt: 1})
				continue
			}
			r.record(LogEntry{Index: j.index, Email: j.recipient.Email, Status: "sent",
				Message: "simulation (dry run)", Worker: worker, Attempt: 1,
				DurationMs: time.Since(start).Milliseconds()})
			continue
		}

		if opts.ReconnectEvery > 0 && sentOnConn >= opts.ReconnectEvery && conn != nil {
			_ = conn.Close()
			conn, sentOnConn = nil, 0
		}

		attempt := 0
		for {
			attempt++
			start := time.Now()

			if conn == nil {
				c, derr := mailer.Dial(opts.SMTP)
				if derr != nil {
					err = derr
				} else {
					conn, sentOnConn = c, 0
					err = nil
				}
			}
			if err == nil {
				err = conn.Send(msg)
			}

			if err == nil {
				sentOnConn++
				r.record(LogEntry{
					Index: j.index, Email: j.recipient.Email, Status: "sent",
					Worker: worker, Attempt: attempt,
					DurationMs: time.Since(start).Milliseconds(),
				})
				break
			}

			code := mailer.StatusCode(err)
			permanent := mailer.IsPermanent(err)
			// Anything that is not a clean 4xx/5xx reply means the session is
			// suspect: drop it so the next attempt reconnects.
			if code == 0 {
				if conn != nil {
					_ = conn.Close()
					conn = nil
				}
			}

			if permanent || attempt > opts.MaxRetries {
				r.record(LogEntry{
					Index: j.index, Email: j.recipient.Email, Status: "failed",
					Message: err.Error(), Code: code, Worker: worker, Attempt: attempt,
					DurationMs: time.Since(start).Milliseconds(),
				})
				if opts.StopOnError {
					r.fail(fmt.Sprintf("campaign stopped after a failure on %s: %v", j.recipient.Email, err))
				}
				break
			}

			r.record(LogEntry{
				Index: j.index, Email: j.recipient.Email, Status: "retry",
				Message: err.Error(), Code: code, Worker: worker, Attempt: attempt,
				DurationMs: time.Since(start).Milliseconds(),
			})
			backoff := time.Duration(attempt*attempt) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (r *Runner) render(opts Options, j job) (*mailer.Message, error) {
	ctx := tmpl.NewContext(j.recipient.Fields, j.index, len(opts.Recipients), opts.IndexStart, opts.Seed)

	subject, _ := tmpl.Render(opts.Subject, ctx)
	html, _ := tmpl.Render(opts.HTML, ctx)
	text, _ := tmpl.Render(opts.Text, ctx)

	headers := make(map[string]string, len(opts.Headers))
	for k, v := range opts.Headers {
		rendered, _ := tmpl.Render(v, ctx)
		headers[k] = rendered
	}

	toName, _ := tmpl.Render(j.recipient.Name, ctx)
	return &mailer.Message{
		FromName:    opts.SMTP.FromName,
		FromEmail:   opts.SMTP.FromEmail,
		ToName:      toName,
		ToEmail:     j.recipient.Email,
		ReplyTo:     opts.SMTP.ReplyTo,
		Subject:     subject,
		HTML:        html,
		Text:        text,
		Attachments: opts.Attachments,
		Headers:     headers,
		Date:        time.Now(),
	}, nil
}

func (r *Runner) record(e LogEntry) {
	e.At = time.Now().Format(time.RFC3339)

	r.mu.Lock()
	r.logs = append(r.logs, e)
	switch e.Status {
	case "sent":
		r.stats.Sent++
	case "failed":
		r.stats.Failed++
	case "retry":
		r.stats.Retries++
	}
	r.mu.Unlock()

	r.broadcast(Event{Type: "log", Log: &e, Stats: r.Snapshot()})
}

func (r *Runner) fail(msg string) {
	r.mu.Lock()
	if r.stats.State != StateFailed {
		r.stats.State = StateFailed
		r.stats.Error = msg
	}
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Snapshot returns the current counters, with elapsed time, rate and ETA
// computed on the fly.
func (r *Runner) Snapshot() *Stats {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.stats
	processed := s.Sent + s.Failed
	s.Pending = s.Total - processed
	if s.Total > 0 {
		s.Progress = float64(processed) / float64(s.Total) * 100
	}
	if !r.started.IsZero() && s.State != StateIdle {
		elapsed := time.Since(r.started)
		s.ElapsedMs = elapsed.Milliseconds()
		if mins := elapsed.Minutes(); mins > 0 {
			s.RatePerMin = float64(processed) / mins
		}
		if s.RatePerMin > 0 && s.Pending > 0 && s.State == StateRunning {
			s.ETASeconds = int(float64(s.Pending) / s.RatePerMin * 60)
		}
	}
	return &s
}

// Logs returns the delivery journal, optionally only the tail.
func (r *Runner) Logs(limit int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit > 0 && len(r.logs) > limit {
		out := make([]LogEntry, limit)
		copy(out, r.logs[len(r.logs)-limit:])
		return out
	}
	out := make([]LogEntry, len(r.logs))
	copy(out, r.logs)
	return out
}

// Pause holds every worker at the next message boundary.
func (r *Runner) Pause() {
	r.mu.Lock()
	if !r.running || r.stats.State != StateRunning {
		r.mu.Unlock()
		return
	}
	r.stats.State = StatePaused
	g := r.gate
	r.mu.Unlock()

	g.close()
	r.broadcast(Event{Type: "state", Stats: r.Snapshot(), Text: "campaign paused"})
}

// Resume releases a paused campaign.
func (r *Runner) Resume() {
	r.mu.Lock()
	if !r.running || r.stats.State != StatePaused {
		r.mu.Unlock()
		return
	}
	r.stats.State = StateRunning
	g := r.gate
	r.mu.Unlock()

	g.open()
	r.broadcast(Event{Type: "state", Stats: r.Snapshot(), Text: "campaign resumed"})
}

// Stop cancels the campaign; messages already handed to a worker finish first.
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	g := r.gate
	r.mu.Unlock()

	if g != nil {
		g.open() // a paused campaign must be released before it can unwind
	}
	if cancel != nil {
		cancel()
	}
}

// Running reports whether a campaign is in flight.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// ReportHeader is the canonical column set of the delivery report. The UI
// language decides which labels are actually written out.
var ReportHeader = []string{"index", "email", "status", "code", "attempts", "duration_ms", "timestamp", "message"}

// Report renders the delivery journal as CSV-ready rows, dropping the
// intermediate retry lines so each recipient appears exactly once.
func (r *Runner) Report() ([]string, [][]string) {
	header := append([]string(nil), ReportHeader...)
	logs := r.Logs(0)
	rows := make([][]string, 0, len(logs))
	for _, l := range logs {
		if l.Status == "retry" {
			continue
		}
		rows = append(rows, []string{
			strconv.Itoa(l.Index), l.Email, l.Status, strconv.Itoa(l.Code),
			strconv.Itoa(l.Attempt), strconv.FormatInt(l.DurationMs, 10), l.At, l.Message,
		})
	}
	return header, rows
}

// Subscribe registers a listener for live events. The returned function must be
// called to release it.
func (r *Runner) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	r.mu.Lock()
	id := r.next
	r.next++
	r.subs[id] = ch
	r.mu.Unlock()

	return ch, func() {
		r.mu.Lock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
		r.mu.Unlock()
	}
}

func (r *Runner) broadcast(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- e:
		default:
			// A listener that cannot keep up is skipped rather than allowed to
			// block the workers; the periodic stats event resynchronises it.
		}
	}
}

// gate blocks workers while the campaign is paused.
type gate struct {
	mu sync.Mutex
	ch chan struct{}
}

func newGate() *gate {
	g := &gate{ch: make(chan struct{})}
	close(g.ch) // starts open
	return g
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.ch: // currently open
		g.ch = make(chan struct{})
	default:
	}
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.ch: // already open
	default:
		close(g.ch)
	}
}

// pacer enforces the global rate limit and the batch cooldown. Slots are
// reserved under a mutex, then awaited outside it so workers never serialise on
// each other's sleep.
type pacer struct {
	mu         sync.Mutex
	interval   time.Duration
	next       time.Time
	count      int
	batchSize  int
	batchPause time.Duration
}

func newPacer(o Options) *pacer {
	p := &pacer{batchSize: o.BatchSize, batchPause: time.Duration(o.BatchPauseSeconds) * time.Second}
	if o.RatePerMinute > 0 {
		p.interval = time.Minute / time.Duration(o.RatePerMinute)
	}
	return p
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	slot := p.next
	p.next = slot.Add(p.interval)
	p.count++
	if p.batchSize > 0 && p.batchPause > 0 && p.count%p.batchSize == 0 {
		p.next = p.next.Add(p.batchPause)
	}
	p.mu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
