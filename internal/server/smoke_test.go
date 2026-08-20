package server

// Smoke test: stands up the real HTTP handler against a fake SMTP server and
// drives every API route end to end, reporting PASS/FAIL per tool. It needs no
// live relay and no network beyond loopback.
//
//	go test ./internal/server/ -run TestSmoke -v
//
// Each t.Run below is one "tool"; -v prints a PASS/FAIL line for each.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aprilox/blastsmtp/internal/mailer"
	"github.com/aprilox/blastsmtp/internal/store"
)

func TestSmoke(t *testing.T) {
	smtp := newFakeSMTP(t)
	h := newHarness(t)
	cfg := smtp.config()

	t.Run("ui served with token injected", func(t *testing.T) {
		status, body := h.raw(t, "GET", "/", "", nil)
		wantStatus(t, status, 200)
		if strings.Contains(string(body), "__BLAST_TOKEN__") {
			t.Fatal("token placeholder was not replaced in the UI")
		}
		if !strings.Contains(string(body), h.token) {
			t.Fatal("token not injected into the UI")
		}
	})

	t.Run("guard rejects missing token", func(t *testing.T) {
		resp, err := http.Get(h.srv.URL + "/api/config")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		wantStatus(t, resp.StatusCode, 401)
	})

	t.Run("guard rejects cross-origin", func(t *testing.T) {
		req, _ := http.NewRequest("GET", h.srv.URL+"/api/config", nil)
		req.Header.Set("X-Blast-Token", h.token)
		req.Header.Set("Origin", "http://evil.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		wantStatus(t, resp.StatusCode, 403)
	})

	t.Run("config get", func(t *testing.T) {
		var out struct {
			ConfigPath string `json:"configPath"`
			Version    string `json:"version"`
		}
		h.getJSON(t, "GET", "/api/config", nil, &out)
		if out.ConfigPath == "" || out.Version == "" {
			t.Fatalf("empty config response: %+v", out)
		}
	})

	t.Run("config save round-trip", func(t *testing.T) {
		data := store.Defaults()
		data.Draft.Subject = "smoke draft"
		var out map[string]any
		h.getJSON(t, "POST", "/api/config", data, &out)
		if out["ok"] != true {
			t.Fatalf("save not ok: %+v", out)
		}
		var back struct {
			Config store.Data `json:"config"`
		}
		h.getJSON(t, "GET", "/api/config", nil, &back)
		if back.Config.Draft.Subject != "smoke draft" {
			t.Fatalf("draft did not persist: %q", back.Config.Draft.Subject)
		}
	})

	t.Run("smtp test probe", func(t *testing.T) {
		var probe mailer.Probe
		h.getJSON(t, "POST", "/api/smtp/test", cfg, &probe)
		if !probe.OK {
			t.Fatalf("probe failed: %s", probe.Error)
		}
	})

	t.Run("recipients upload/get/clear", func(t *testing.T) {
		csv := "prenom;email;ville\nAmelie;amelie@example.com;Lyon\nBob;bob@example.com;Paris\nBob;bob@example.com;Paris\n"
		var up struct {
			Loaded     bool     `json:"loaded"`
			Count      int      `json:"count"`
			Columns    []string `json:"columns"`
			Duplicates int      `json:"duplicates"`
		}
		h.upload(t, "/api/recipients", "file", "list.csv", []byte(csv), &up)
		if !up.Loaded || up.Count != 2 || up.Duplicates != 1 {
			t.Fatalf("unexpected import: %+v", up)
		}

		var got struct {
			Count int `json:"count"`
		}
		h.getJSON(t, "GET", "/api/recipients", nil, &got)
		if got.Count != 2 {
			t.Fatalf("get recipients count = %d", got.Count)
		}
		// Leave the list loaded for preview/campaign; a dedicated clear check:
		var cleared struct {
			Loaded bool `json:"loaded"`
		}
		h.getJSON(t, "DELETE", "/api/recipients", nil, &cleared)
		if cleared.Loaded {
			t.Fatal("list still loaded after clear")
		}
		// Re-import so later tools have data to work with.
		h.upload(t, "/api/recipients", "file", "list.csv", []byte(csv), &up)
	})

	t.Run("attachments upload/inline/delete", func(t *testing.T) {
		png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 1, 2, 3}
		var list []struct {
			Filename  string `json:"filename"`
			Inline    bool   `json:"inline"`
			ContentID string `json:"contentId"`
		}
		h.uploadField(t, "/api/attachments", "files", "logo.png", "image/png", png, &list)
		if len(list) != 1 || list[0].ContentID == "" {
			t.Fatalf("upload result: %+v", list)
		}

		h.getJSON(t, "POST", "/api/attachments/logo.png/inline", nil, &list)
		if !list[0].Inline {
			t.Fatal("attachment did not toggle to inline")
		}

		h.getJSON(t, "DELETE", "/api/attachments/logo.png", nil, &list)
		if len(list) != 0 {
			t.Fatalf("attachment not deleted: %+v", list)
		}
	})

	t.Run("preview renders variables", func(t *testing.T) {
		req := map[string]any{
			"subject": "Hi {{capitalize:prenom}}",
			"html":    "<p>#{{index}} — {{ville}} — {{missingvar}}</p>",
			"index":   1,
		}
		var out struct {
			Subject string   `json:"subject"`
			HTML    string   `json:"html"`
			Missing []string `json:"missing"`
			To      string   `json:"to"`
		}
		h.getJSON(t, "POST", "/api/preview", req, &out)
		if out.Subject != "Hi Amelie" {
			t.Fatalf("subject = %q", out.Subject)
		}
		if !strings.Contains(out.HTML, "Lyon") {
			t.Fatalf("html = %q", out.HTML)
		}
		if len(out.Missing) == 0 {
			t.Fatal("missing variable not reported")
		}
	})

	t.Run("send test", func(t *testing.T) {
		req := map[string]any{
			"smtp":    cfg,
			"to":      "check@example.com",
			"subject": "Test {{prenom}}",
			"html":    "<p>hello {{prenom}}</p>",
			"index":   1,
		}
		var out struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		h.getJSON(t, "POST", "/api/send-test", req, &out)
		if !out.OK {
			t.Fatalf("send-test failed: %s", out.Error)
		}
		if n := smtp.count(); n < 1 {
			t.Fatalf("fake SMTP received %d messages", n)
		}
	})

	t.Run("campaign dry run completes", func(t *testing.T) {
		h.startCampaign(t, cfg, true)
		st := h.waitDone(t)
		if st.State != "done" || st.Sent != 2 {
			t.Fatalf("dry run state=%s sent=%d", st.State, st.Sent)
		}
	})

	t.Run("campaign real send completes", func(t *testing.T) {
		before := smtp.count()
		h.startCampaign(t, cfg, false)
		st := h.waitDone(t)
		if st.State != "done" || st.Sent != 2 {
			t.Fatalf("campaign state=%s sent=%d failed=%d", st.State, st.Sent, st.Failed)
		}
		if got := smtp.count() - before; got != 2 {
			t.Fatalf("fake SMTP received %d campaign messages, want 2", got)
		}
	})

	t.Run("campaign report csv", func(t *testing.T) {
		status, body := h.raw(t, "GET", "/api/campaign/report.csv", "", nil)
		wantStatus(t, status, 200)
		if lines := strings.Count(strings.TrimSpace(string(body)), "\n"); lines < 2 {
			t.Fatalf("report has too few rows:\n%s", body)
		}
	})

	t.Run("campaign state and stream", func(t *testing.T) {
		var out struct {
			Stats struct {
				State string `json:"state"`
			} `json:"stats"`
		}
		h.getJSON(t, "GET", "/api/campaign/state", nil, &out)
		if out.Stats.State == "" {
			t.Fatal("empty state")
		}
		// SSE: the handler pushes an initial "state" event immediately.
		if ev := h.firstSSE(t); !strings.Contains(ev, "\"state\"") {
			t.Fatalf("first SSE event: %q", ev)
		}
	})

	t.Run("campaign pause/resume/stop are idempotent when idle", func(t *testing.T) {
		for _, path := range []string{"/api/campaign/pause", "/api/campaign/resume", "/api/campaign/stop"} {
			var st struct {
				State string `json:"state"`
			}
			h.getJSON(t, "POST", path, nil, &st)
		}
	})
}

// ------------------------------------------------------------------ harness

type harness struct {
	srv    *httptest.Server
	token  string
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><body>__BLAST_TOKEN__</body></html>")},
	}
	st, err := store.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(assets, st)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{srv: ts, token: srv.Token(), client: ts.Client()}
}

func (h *harness) raw(t *testing.T, method, path, contentType string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Blast-Token", h.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// getJSON sends body (nil or a JSON-encodable value) and decodes the response
// into out. It fails the test on any non-2xx status.
func (h *harness) getJSON(t *testing.T, method, path string, body, out any) {
	t.Helper()
	var r io.Reader
	ct := ""
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r, ct = bytes.NewReader(buf), "application/json"
	}
	status, data := h.raw(t, method, path, ct, r)
	if status < 200 || status >= 300 {
		t.Fatalf("%s %s -> %d: %s", method, path, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("decode %s %s: %v (body: %s)", method, path, err, data)
		}
	}
}

func (h *harness) upload(t *testing.T, path, field, filename string, content []byte, out any) {
	t.Helper()
	h.uploadField(t, path, field, filename, "", content, out)
}

func (h *harness) uploadField(t *testing.T, path, field, filename, ctype string, content []byte, out any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	var (
		fw  io.Writer
		err error
	)
	if ctype != "" {
		hdr := make(map[string][]string)
		hdr["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename)}
		hdr["Content-Type"] = []string{ctype}
		fw, err = mw.CreatePart(hdr)
	} else {
		fw, err = mw.CreateFormFile(field, filename)
	}
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.Close()
	status, data := h.raw(t, "POST", path, mw.FormDataContentType(), &buf)
	if status < 200 || status >= 300 {
		t.Fatalf("upload %s -> %d: %s", path, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("decode upload %s: %v (body: %s)", path, err, data)
		}
	}
}

type campaignState struct {
	State  string `json:"state"`
	Sent   int    `json:"sent"`
	Failed int    `json:"failed"`
}

func (h *harness) startCampaign(t *testing.T, cfg mailer.Config, dryRun bool) {
	t.Helper()
	req := map[string]any{
		"smtp":    cfg,
		"subject": "Hello {{prenom}}",
		"html":    "<p>Hi {{prenom}} from {{ville}}</p>",
		"dryRun":  dryRun,
		"sending": store.Sending{Workers: 2, MaxRetries: 0, IndexStart: 1},
	}
	var out struct {
		OK bool `json:"ok"`
	}
	h.getJSON(t, "POST", "/api/campaign/start", req, &out)
	if !out.OK {
		t.Fatal("campaign did not start")
	}
}

func (h *harness) waitDone(t *testing.T) campaignState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var out struct {
			Stats campaignState `json:"stats"`
		}
		h.getJSON(t, "GET", "/api/campaign/state", nil, &out)
		switch out.Stats.State {
		case "done", "failed", "stopped":
			return out.Stats
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("campaign did not finish within 10s")
	return campaignState{}
}

func (h *harness) firstSSE(t *testing.T) string {
	t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/campaign/stream", nil)
	req.Header.Set("X-Blast-Token", h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") {
			return line
		}
	}
	t.Fatal("no SSE data event received")
	return ""
}

func wantStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// --------------------------------------------------- fake SMTP (test double)

// fakeSMTP is a minimal ESMTP server: enough of the protocol to drive the
// client through EHLO, AUTH, MAIL/RCPT/DATA and QUIT across many sessions.
type fakeSMTP struct {
	ln net.Listener
	mu sync.Mutex
	n  int // messages accepted
	wg sync.WaitGroup
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *fakeSMTP) config() mailer.Config {
	return mailer.Config{
		Host: "127.0.0.1", Port: s.ln.Addr().(*net.TCPAddr).Port,
		Username: "user", Password: "pass",
		Encryption: mailer.EncryptionNone, AuthMethod: mailer.AuthPlain,
		FromEmail: "sender@example.com", FromName: "Sender",
		AllowInsecureAuth: true, TimeoutSeconds: 5,
	}
}

func (s *fakeSMTP) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	say := func(f string, a ...any) { fmt.Fprintf(w, f+"\r\n", a...); w.Flush() }
	read := func() (string, bool) {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}

	say("220 fake ESMTP ready")
	for {
		line, ok := read()
		if !ok {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			say("250-fake greets you")
			say("250-AUTH PLAIN LOGIN")
			say("250-8BITMIME")
			say("250 SIZE 26214400")
		case strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			if len(strings.Fields(line)) < 3 {
				say("334 ")
				read()
			}
			say("235 authenticated")
		case strings.HasPrefix(cmd, "AUTH LOGIN"):
			say("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
			read()
			say("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
			read()
			say("235 authenticated")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			say("250 ok")
		case cmd == "DATA":
			say("354 go ahead")
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
			}
			s.mu.Lock()
			s.n++
			s.mu.Unlock()
			say("250 queued")
		case cmd == "RSET":
			say("250 flushed")
		case cmd == "NOOP":
			say("250 ok")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("500 unknown command")
		}
	}
}
