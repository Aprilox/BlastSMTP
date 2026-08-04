package mailer

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a minimal ESMTP server: just enough of the protocol to exercise
// the client end to end without touching the network.
type fakeSMTP struct {
	ln         net.Listener
	mu         sync.Mutex
	messages   []string
	user, pass string
	rejectRcpt string // recipient answered with a permanent 550
	wg         sync.WaitGroup
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, user: "moi", pass: "secret"}
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

func (s *fakeSMTP) port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

func (s *fakeSMTP) config() Config {
	return Config{
		Host: "127.0.0.1", Port: s.port(),
		Username: s.user, Password: s.pass,
		Encryption: EncryptionNone, AuthMethod: AuthAuto,
		FromEmail: "moi@exemple.fr", FromName: "Moi",
		AllowInsecureAuth: true, TimeoutSeconds: 5,
	}
}

func (s *fakeSMTP) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	say := func(format string, a ...any) {
		fmt.Fprintf(w, format+"\r\n", a...)
		w.Flush()
	}
	readLine := func() (string, bool) {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}

	say("220 fake ESMTP ready")
	for {
		line, ok := readLine()
		if !ok {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			say("250-fake greets you")
			say("250-AUTH PLAIN LOGIN")
			say("250-8BITMIME")
			say("250 SIZE 10485760")

		case strings.HasPrefix(cmd, "HELO"):
			say("250 fake")

		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			fields := strings.Fields(line)
			payload := ""
			if len(fields) >= 3 {
				payload = fields[2]
			} else {
				say("334 ")
				payload, _ = readLine()
			}
			raw, _ := base64.StdEncoding.DecodeString(payload)
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.user && parts[2] == s.pass {
				say("235 authenticated")
			} else {
				say("535 bad credentials")
			}

		case strings.HasPrefix(cmd, "AUTH LOGIN"):
			say("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
			u, _ := readLine()
			say("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
			p, _ := readLine()
			du, _ := base64.StdEncoding.DecodeString(u)
			dp, _ := base64.StdEncoding.DecodeString(p)
			if string(du) == s.user && string(dp) == s.pass {
				say("235 authenticated")
			} else {
				say("535 bad credentials")
			}

		case strings.HasPrefix(cmd, "MAIL FROM"):
			say("250 sender ok")

		case strings.HasPrefix(cmd, "RCPT TO"):
			if s.rejectRcpt != "" && strings.Contains(line, s.rejectRcpt) {
				say("550 no such mailbox")
			} else {
				say("250 recipient ok")
			}

		case cmd == "DATA":
			say("354 go ahead")
			var body strings.Builder
			for {
				l, ok := readLine()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				// Undo dot-stuffing, exactly like a real server.
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				body.WriteString(l)
				body.WriteString("\n")
			}
			s.mu.Lock()
			s.messages = append(s.messages, body.String())
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

func TestDialAndSend(t *testing.T) {
	srv := newFakeSMTP(t)
	conn, err := Dial(srv.config())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	err = conn.Send(&Message{
		ToEmail: "toi@exemple.fr", ToName: "Toi",
		Subject: "Objet accentué é",
		Text:    "Bonjour\n.une ligne commençant par un point\nfin",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if conn.Sent() != 1 {
		t.Errorf("Sent() = %d", conn.Sent())
	}

	got := srv.got()
	if len(got) != 1 {
		t.Fatalf("server received %d messages", len(got))
	}
	body := got[0]
	if !strings.Contains(body, "To: \"Toi\" <toi@exemple.fr>") && !strings.Contains(body, "To: Toi <toi@exemple.fr>") {
		t.Errorf("To header missing from:\n%s", body)
	}
	// Dot-stuffing must be transparent: the leading dot survives the round trip.
	if !strings.Contains(body, "\n.une ligne") {
		t.Errorf("the dot-stuffed line did not round-trip:\n%s", body)
	}
}

func TestSendMultipleOnOneSession(t *testing.T) {
	srv := newFakeSMTP(t)
	conn, err := Dial(srv.config())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for i := range 5 {
		err := conn.Send(&Message{
			ToEmail: fmt.Sprintf("dest%d@exemple.fr", i),
			Subject: "n°" + strconv.Itoa(i),
			Text:    "corps",
		})
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if n := len(srv.got()); n != 5 {
		t.Errorf("server received %d messages, want 5", n)
	}
}

func TestPermanentRejectionIsDetected(t *testing.T) {
	srv := newFakeSMTP(t)
	srv.rejectRcpt = "inconnu@exemple.fr"

	conn, err := Dial(srv.config())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	err = conn.Send(&Message{ToEmail: "inconnu@exemple.fr", Subject: "x", Text: "y"})
	if err == nil {
		t.Fatal("a rejected recipient returned no error")
	}
	if !IsPermanent(err) {
		t.Errorf("IsPermanent(%v) = false, want true", err)
	}
	if code := StatusCode(err); code != 550 {
		t.Errorf("StatusCode = %d, want 550", code)
	}
	// The session must stay usable after a rejection so the worker can carry on.
	if err := conn.Send(&Message{ToEmail: "ok@exemple.fr", Subject: "x", Text: "y"}); err != nil {
		t.Errorf("session unusable after a rejection: %v", err)
	}
}

func TestLoginAuthMechanism(t *testing.T) {
	srv := newFakeSMTP(t)
	cfg := srv.config()
	cfg.AuthMethod = AuthLogin

	conn, err := Dial(cfg)
	if err != nil {
		t.Fatalf("AUTH LOGIN: %v", err)
	}
	conn.Close()
}

func TestBadCredentialsFail(t *testing.T) {
	srv := newFakeSMTP(t)
	cfg := srv.config()
	cfg.Password = "mauvais"

	if _, err := Dial(cfg); err == nil {
		t.Fatal("Dial succeeded with a wrong password")
	}
}

func TestTestProbeReportsExtensions(t *testing.T) {
	srv := newFakeSMTP(t)
	p := Test(srv.config())
	if !p.OK {
		t.Fatalf("probe failed: %s", p.Error)
	}
	if p.MaxSize != 10485760 {
		t.Errorf("MaxSize = %d", p.MaxSize)
	}
	if !strings.Contains(strings.Join(p.Extensions, ","), "8BITMIME") {
		t.Errorf("extensions = %v", p.Extensions)
	}
	if !strings.Contains(p.Auth, "PLAIN") {
		t.Errorf("auth = %q", p.Auth)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	cases := map[string]Config{
		"no host":       {Port: 587, FromEmail: "a@b.fr"},
		"bad port":      {Host: "x", Port: 0, FromEmail: "a@b.fr"},
		"no sender":     {Host: "x", Port: 587},
		"bad crypto":    {Host: "x", Port: 587, FromEmail: "a@b.fr", Encryption: "quantum"},
		"bad auth mode": {Host: "x", Port: 587, FromEmail: "a@b.fr", AuthMethod: "telepathy"},
	}
	for name, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() returned nil", name)
		}
	}
}
