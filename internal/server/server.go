// Package server exposes the local HTTP API and serves the embedded UI.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aprilox/blastsmtp/internal/campaign"
	"github.com/aprilox/blastsmtp/internal/mailer"
	"github.com/aprilox/blastsmtp/internal/recipients"
	"github.com/aprilox/blastsmtp/internal/store"
	"github.com/aprilox/blastsmtp/internal/tmpl"
)

// Version is displayed in the UI; main overrides it at startup.
var Version = "dev"

const (
	maxListBytes        = 64 << 20 // 64 MiB of addresses is ~1M lines
	maxAttachmentsBytes = 25 << 20 // most relays reject beyond this anyway
	previewRecipients   = 200
)

// Server holds the state shared by every request: the imported list, the
// staged attachments and the campaign runner.
type Server struct {
	mux    *http.ServeMux
	assets fs.FS
	store  *store.Store
	runner *campaign.Runner
	token  string

	mu          sync.RWMutex
	list        *recipients.Result
	listName    string
	attachments []mailer.Attachment
}

// New wires the HTTP handlers. assets must contain the UI files at its root.
func New(assets fs.FS, st *store.Store) (*Server, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	s := &Server{
		mux:    http.NewServeMux(),
		assets: assets,
		store:  st,
		runner: campaign.New(),
		token:  token,
	}
	s.routes()
	return s, nil
}

// Token is the shared secret the UI must present on every API call.
func (s *Server) Token() string { return s.token }

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleUI)

	api := func(pattern string, h http.HandlerFunc) {
		s.mux.Handle(pattern, s.guard(h))
	}
	api("GET /api/config", s.handleGetConfig)
	api("POST /api/config", s.handleSaveConfig)
	api("POST /api/smtp/test", s.handleSMTPTest)

	api("POST /api/recipients", s.handleUploadRecipients)
	api("GET /api/recipients", s.handleGetRecipients)
	api("DELETE /api/recipients", s.handleClearRecipients)

	api("GET /api/attachments", s.handleGetAttachments)
	api("POST /api/attachments", s.handleUploadAttachments)
	api("DELETE /api/attachments/{name}", s.handleDeleteAttachment)
	api("POST /api/attachments/{name}/inline", s.handleToggleInline)

	api("POST /api/preview", s.handlePreview)
	api("POST /api/send-test", s.handleSendTest)

	api("POST /api/campaign/start", s.handleStart)
	api("POST /api/campaign/pause", s.handlePause)
	api("POST /api/campaign/resume", s.handleResume)
	api("POST /api/campaign/stop", s.handleStop)
	api("GET /api/campaign/state", s.handleState)
	api("GET /api/campaign/stream", s.handleStream)
	api("GET /api/campaign/report.csv", s.handleReport)
}

// guard rejects requests that do not carry the session token, and requests
// coming from another origin. The server only listens on the loopback
// interface, but any page in the user's browser could otherwise reach it.
func (s *Server) guard(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(origin, r.Host) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		token := r.Header.Get("X-Blast-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			http.Error(w, "invalid session token, reopen the URL printed in the terminal", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func (s *Server) sameOrigin(origin, host string) bool {
	if i := strings.Index(origin, "//"); i >= 0 {
		origin = origin[i+2:]
	}
	return strings.EqualFold(origin, host)
}

// ---------------------------------------------------------------- UI assets

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	data, err := fs.ReadFile(s.assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	if name == "index.html" {
		// The token is injected here so the UI never has to keep it in the URL.
		data = []byte(strings.Replace(string(data), "__BLAST_TOKEN__", s.token, 1))
	}
	_, _ = w.Write(data)
}

// ------------------------------------------------------------------- config

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{
		"config":     data,
		"configPath": s.store.Path(),
		"version":    Version,
	})
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var data store.Data
	if err := readJSON(r, &data); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.Save(data); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "configPath": s.store.Path()})
}

func (s *Server) handleSMTPTest(w http.ResponseWriter, r *http.Request) {
	var cfg mailer.Config
	if err := readJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, mailer.Test(cfg))
}

// --------------------------------------------------------------- recipients

func (s *Server) handleUploadRecipients(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no file received: %w", err))
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxListBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := recipients.Parse(header.Filename, data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.mu.Lock()
	s.list = result
	s.listName = header.Filename
	s.mu.Unlock()

	writeJSON(w, s.listSummary())
}

func (s *Server) handleGetRecipients(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.listSummary())
}

func (s *Server) handleClearRecipients(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.list, s.listName = nil, ""
	s.mu.Unlock()
	writeJSON(w, s.listSummary())
}

// listSummary describes the imported list without shipping a million rows to
// the browser: only the first page of recipients is included.
func (s *Server) listSummary() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.list == nil {
		return map[string]any{"loaded": false, "count": 0}
	}
	preview := s.list.Recipients
	if len(preview) > previewRecipients {
		preview = preview[:previewRecipients]
	}
	return map[string]any{
		"loaded":     true,
		"filename":   s.listName,
		"count":      len(s.list.Recipients),
		"columns":    s.list.Columns,
		"rejected":   s.list.Rejected,
		"duplicates": s.list.Duplicates,
		"delimiter":  s.list.Delimiter,
		"hasHeader":  s.list.HasHeader,
		"format":     s.list.Format,
		"preview":    preview,
	}
}

// -------------------------------------------------------------- attachments

func (s *Server) handleGetAttachments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.attachmentList())
}

func (s *Server) handleUploadAttachments(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxAttachmentsBytes); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no file received"))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for _, a := range s.attachments {
		total += len(a.Content)
	}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		content, err := io.ReadAll(io.LimitReader(f, maxAttachmentsBytes))
		f.Close()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		total += len(content)
		if total > maxAttachmentsBytes {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Errorf("attachments exceed %d MB in total", maxAttachmentsBytes>>20))
			return
		}
		name := filepath.Base(fh.Filename)
		ct := fh.Header.Get("Content-Type")
		if ct == "" || ct == "application/octet-stream" {
			if guess := mime.TypeByExtension(filepath.Ext(name)); guess != "" {
				ct = guess
			}
		}
		// Images get a Content-ID up front so they can be switched to inline
		// and referenced from the HTML with cid: without re-uploading.
		cid := ""
		if strings.HasPrefix(ct, "image/") {
			cid = tmpl.NormalizeKey(name) + ".blast"
		}
		s.attachments = append(s.attachments, mailer.Attachment{
			Filename:  name,
			MIMEType:  ct,
			Content:   content,
			Inline:    false,
			ContentID: cid,
		})
	}
	writeJSON(w, s.attachmentListLocked())
}

func (s *Server) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	kept := s.attachments[:0]
	for _, a := range s.attachments {
		if a.Filename != name {
			kept = append(kept, a)
		}
	}
	s.attachments = kept
	s.mu.Unlock()
	writeJSON(w, s.attachmentList())
}

func (s *Server) attachmentList() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attachmentListLocked()
}

func (s *Server) attachmentListLocked() []map[string]any {
	out := make([]map[string]any, 0, len(s.attachments))
	for _, a := range s.attachments {
		out = append(out, map[string]any{
			"filename":  a.Filename,
			"mimeType":  a.MIMEType,
			"size":      len(a.Content),
			"inline":    a.Inline,
			"contentId": a.ContentID,
		})
	}
	return out
}

// handleToggleInline flips an image between "attached file" and "embedded in
// the HTML through cid:".
func (s *Server) handleToggleInline(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	for i := range s.attachments {
		if s.attachments[i].Filename != name {
			continue
		}
		s.attachments[i].Inline = !s.attachments[i].Inline
		if s.attachments[i].Inline && s.attachments[i].ContentID == "" {
			s.attachments[i].ContentID = tmpl.NormalizeKey(name) + ".blast"
		}
	}
	s.mu.Unlock()
	writeJSON(w, s.attachmentList())
}

// ------------------------------------------------------------------ preview

type previewRequest struct {
	Subject    string            `json:"subject"`
	HTML       string            `json:"html"`
	Text       string            `json:"text"`
	Headers    map[string]string `json:"headers"`
	Index      int               `json:"index"`
	IndexStart int               `json:"indexStart"`
	Seed       int64             `json:"seed"`
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.mu.RLock()
	list := s.list
	s.mu.RUnlock()

	fields := map[string]string{"email": "exemple@domaine.com", "name": "Exemple"}
	total, index := 1, 1
	if list != nil && len(list.Recipients) > 0 {
		index = clamp(req.Index, 1, len(list.Recipients))
		fields = list.Recipients[index-1].Fields
		total = len(list.Recipients)
	}
	if req.IndexStart == 0 {
		req.IndexStart = 1
	}

	ctx := tmpl.NewContext(fields, index, total, req.IndexStart, req.Seed)
	subject, m1 := tmpl.Render(req.Subject, ctx)
	html, m2 := tmpl.Render(req.HTML, ctx)
	text, m3 := tmpl.Render(req.Text, ctx)

	writeJSON(w, map[string]any{
		"index":   index,
		"total":   total,
		"to":      fields["email"],
		"subject": subject,
		"html":    html,
		"text":    text,
		"missing": dedupe(append(append(m1, m2...), m3...)),
		"fields":  fields,
	})
}

type sendTestRequest struct {
	SMTP       mailer.Config     `json:"smtp"`
	To         string            `json:"to"`
	Subject    string            `json:"subject"`
	HTML       string            `json:"html"`
	Text       string            `json:"text"`
	Headers    map[string]string `json:"headers"`
	Index      int               `json:"index"`
	IndexStart int               `json:"indexStart"`
}

// handleSendTest delivers a single message, rendered with the data of one real
// recipient, to an address of the operator's choosing.
func (s *Server) handleSendTest(w http.ResponseWriter, r *http.Request) {
	var req sendTestRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.To) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a test address is required"))
		return
	}

	s.mu.RLock()
	list := s.list
	attachments := append([]mailer.Attachment(nil), s.attachments...)
	s.mu.RUnlock()

	fields := map[string]string{"email": req.To, "name": "Test"}
	total, index := 1, 1
	if list != nil && len(list.Recipients) > 0 {
		index = clamp(req.Index, 1, len(list.Recipients))
		fields = list.Recipients[index-1].Fields
		total = len(list.Recipients)
	}
	if req.IndexStart == 0 {
		req.IndexStart = 1
	}

	ctx := tmpl.NewContext(fields, index, total, req.IndexStart, time.Now().UnixNano())
	subject, _ := tmpl.Render(req.Subject, ctx)
	html, _ := tmpl.Render(req.HTML, ctx)
	text, _ := tmpl.Render(req.Text, ctx)
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[k], _ = tmpl.Render(v, ctx)
	}

	start := time.Now()
	conn, err := mailer.Dial(req.SMTP)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer conn.Close()

	err = conn.Send(&mailer.Message{
		FromName: req.SMTP.FromName, FromEmail: req.SMTP.FromEmail,
		ToEmail: req.To, ReplyTo: req.SMTP.ReplyTo,
		Subject: subject, HTML: html, Text: text,
		Attachments: attachments, Headers: headers, Date: time.Now(),
	})
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "code": mailer.StatusCode(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "durationMs": time.Since(start).Milliseconds()})
}

// ----------------------------------------------------------------- campaign

type startRequest struct {
	SMTP        mailer.Config     `json:"smtp"`
	Subject     string            `json:"subject"`
	HTML        string            `json:"html"`
	Text        string            `json:"text"`
	Headers     map[string]string `json:"headers"`
	Sending     store.Sending     `json:"sending"`
	DryRun      bool              `json:"dryRun"`
	Unsubscribe string            `json:"unsubscribe"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.mu.RLock()
	list := s.list
	attachments := append([]mailer.Attachment(nil), s.attachments...)
	s.mu.RUnlock()

	if list == nil || len(list.Recipients) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("import a recipient list first"))
		return
	}
	if strings.TrimSpace(req.Subject) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the subject is empty"))
		return
	}
	if strings.TrimSpace(req.HTML) == "" && strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the message body is empty"))
		return
	}

	headers := map[string]string{}
	for k, v := range req.Headers {
		if k = strings.TrimSpace(k); k != "" {
			headers[k] = v
		}
	}
	// A working unsubscribe path is what separates a mailing from spam; wire it
	// into the headers so clients surface a one-click link.
	if u := strings.TrimSpace(req.Unsubscribe); u != "" {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			headers["List-Unsubscribe"] = "<" + u + ">"
			headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
		} else {
			headers["List-Unsubscribe"] = "<mailto:" + strings.TrimPrefix(u, "mailto:") + ">"
		}
	}

	opts := campaign.Options{
		SMTP:              req.SMTP,
		Subject:           req.Subject,
		HTML:              req.HTML,
		Text:              req.Text,
		Headers:           headers,
		Attachments:       attachments,
		Recipients:        list.Recipients,
		Workers:           req.Sending.Workers,
		RatePerMinute:     req.Sending.RatePerMinute,
		BatchSize:         req.Sending.BatchSize,
		BatchPauseSeconds: req.Sending.BatchPauseSeconds,
		MaxRetries:        req.Sending.MaxRetries,
		ReconnectEvery:    req.Sending.ReconnectEvery,
		IndexStart:        req.Sending.IndexStart,
		StopOnError:       req.Sending.StopOnError,
		DryRun:            req.DryRun,
		Seed:              time.Now().UnixNano(),
	}
	if err := s.runner.Start(opts); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "stats": s.runner.Snapshot()})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.runner.Pause()
	writeJSON(w, s.runner.Snapshot())
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.runner.Resume()
	writeJSON(w, s.runner.Snapshot())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.runner.Stop()
	writeJSON(w, s.runner.Snapshot())
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("logs"))
	if limit == 0 {
		limit = 200
	}
	writeJSON(w, map[string]any{
		"stats": s.runner.Snapshot(),
		"logs":  s.runner.Logs(limit),
	})
}

// handleStream pushes campaign events to the UI over Server-Sent Events.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := s.runner.Subscribe()
	defer unsubscribe()

	send := func(e campaign.Event) bool {
		payload, err := json.Marshal(e)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	send(campaign.Event{Type: "state", Stats: s.runner.Snapshot()})

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case e, ok := <-events:
			if !ok || !send(e) {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// reportHeaderFR mirrors campaign.ReportHeader for the French UI.
var reportHeaderFR = []string{"index", "email", "statut", "code", "tentatives", "duree_ms", "horodatage", "message"}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	header, rows := s.runner.Report()
	stem := "blastsmtp-report"
	if r.URL.Query().Get("lang") == "fr" {
		header = reportHeaderFR
		stem = "blastsmtp-rapport"
	}
	name := fmt.Sprintf("%s-%s.csv", stem, time.Now().Format("2006-01-02-1504"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if err := recipients.WriteReport(w, header, rows); err != nil {
		log.Printf("report: %v", err)
	}
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encoding response: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("malformed request: %w", err)
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Listen binds the HTTP listener, picking a free port when port is 0.
func Listen(host string, port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}
