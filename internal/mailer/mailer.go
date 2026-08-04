// Package mailer builds MIME messages and delivers them over SMTP.
package mailer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// Encryption modes understood by Config.Encryption.
const (
	EncryptionNone     = "none"     // plain TCP, no TLS at all
	EncryptionSTARTTLS = "starttls" // plain TCP upgraded with STARTTLS (587)
	EncryptionSSL      = "ssl"      // TLS from the first byte, aka SMTPS (465)
)

// Authentication mechanisms understood by Config.AuthMethod.
const (
	AuthAuto    = "auto"
	AuthPlain   = "plain"
	AuthLogin   = "login"
	AuthCRAMMD5 = "cram-md5"
	AuthNone    = "none"
)

// Config describes how to reach and authenticate against an SMTP relay.
type Config struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Encryption string `json:"encryption"`
	AuthMethod string `json:"authMethod"`

	FromName  string `json:"fromName"`
	FromEmail string `json:"fromEmail"`
	ReplyTo   string `json:"replyTo"`

	// HELOName overrides the hostname announced in EHLO. Some relays reject
	// the machine's real hostname when it does not resolve publicly.
	HELOName string `json:"heloName"`
	// SkipVerify disables certificate validation. Needed for self-signed
	// relays; it also removes any protection against interception.
	SkipVerify bool `json:"skipVerify"`
	// AllowInsecureAuth permits sending credentials over a connection with no
	// TLS. Off by default because it leaks the password on the wire.
	AllowInsecureAuth bool `json:"allowInsecureAuth"`
	// TimeoutSeconds bounds dialling and the whole SMTP conversation.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

func (c Config) timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

func (c Config) helo() string {
	if c.HELOName != "" {
		return c.HELOName
	}
	if h, err := os.Hostname(); err == nil && h != "" && !strings.ContainsAny(h, " \t") {
		return h
	}
	return "localhost"
}

// Validate reports whether the configuration can be used to open a session.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("SMTP host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid SMTP port %d", c.Port)
	}
	if strings.TrimSpace(c.FromEmail) == "" {
		return errors.New("sender address is required")
	}
	switch c.Encryption {
	case EncryptionNone, EncryptionSTARTTLS, EncryptionSSL, "":
	default:
		return fmt.Errorf("unknown encryption mode %q", c.Encryption)
	}
	switch c.AuthMethod {
	case AuthAuto, AuthPlain, AuthLogin, AuthCRAMMD5, AuthNone, "":
	default:
		return fmt.Errorf("unknown auth method %q", c.AuthMethod)
	}
	return nil
}

// Conn is a live authenticated SMTP session. It is not safe for concurrent use;
// give every worker its own connection.
type Conn struct {
	client *smtp.Client
	// raw is kept so deadlines can be refreshed between messages; setting them
	// on the TCP socket also covers the TLS layer wrapped around it.
	raw  net.Conn
	cfg  Config
	sent int
}

// Dial opens a connection, upgrades it to TLS when requested and authenticates.
func Dial(cfg Config) (*Conn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: cfg.timeout()}

	var (
		conn net.Conn
		err  error
	)
	if cfg.Encryption == EncryptionSSL {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, cfg.tlsConfig())
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connection to %s failed: %w", addr, err)
	}
	// A deadline on the raw socket covers every later read and write, so a
	// silent relay can never hang a worker forever.
	_ = conn.SetDeadline(time.Now().Add(cfg.timeout()))

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	if err := client.Hello(cfg.helo()); err != nil {
		client.Close()
		return nil, fmt.Errorf("EHLO refused: %w", err)
	}

	if cfg.Encryption == EncryptionSTARTTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			client.Close()
			return nil, errors.New("server does not advertise STARTTLS (try SSL/TLS or no encryption)")
		}
		if err := client.StartTLS(cfg.tlsConfig()); err != nil {
			client.Close()
			return nil, fmt.Errorf("STARTTLS failed: %w", err)
		}
	}

	c := &Conn{client: client, raw: conn, cfg: cfg}
	if err := c.authenticate(); err != nil {
		client.Close()
		return nil, err
	}
	return c, nil
}

func (cfg Config) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         cfg.Host,
		InsecureSkipVerify: cfg.SkipVerify,
		MinVersion:         tls.VersionTLS12,
	}
}

func (c *Conn) authenticate() error {
	if c.cfg.AuthMethod == AuthNone || c.cfg.Username == "" {
		return nil
	}
	_, params := c.client.Extension("AUTH")
	mechanisms := strings.ToUpper(params)
	_, isTLS := c.client.TLSConnectionState()
	insecure := c.cfg.AllowInsecureAuth

	method := c.cfg.AuthMethod
	if method == "" || method == AuthAuto {
		switch {
		case strings.Contains(mechanisms, "PLAIN") && (isTLS || insecure):
			method = AuthPlain
		case strings.Contains(mechanisms, "LOGIN"):
			method = AuthLogin
		case strings.Contains(mechanisms, "CRAM-MD5"):
			method = AuthCRAMMD5
		default:
			method = AuthPlain
		}
	}

	var auth smtp.Auth
	switch method {
	case AuthLogin:
		auth = LoginAuth(c.cfg.Username, c.cfg.Password, c.cfg.Host, insecure)
	case AuthCRAMMD5:
		auth = smtp.CRAMMD5Auth(c.cfg.Username, c.cfg.Password)
	default:
		auth = PlainAuth(c.cfg.Username, c.cfg.Password, c.cfg.Host, insecure)
	}

	if err := c.client.Auth(auth); err != nil {
		return fmt.Errorf("authentication failed (%s): %w", method, err)
	}
	return nil
}

// Send delivers one message on the open session.
func (c *Conn) Send(m *Message) error {
	if m.FromEmail == "" {
		m.FromEmail = c.cfg.FromEmail
	}
	if m.FromName == "" {
		m.FromName = c.cfg.FromName
	}
	if m.ReplyTo == "" {
		m.ReplyTo = c.cfg.ReplyTo
	}
	raw, err := m.Build()
	if err != nil {
		return fmt.Errorf("building message: %w", err)
	}

	// Refresh the deadline for this exchange rather than letting the dial-time
	// one expire midway through a long campaign.
	c.setDeadline()

	if err := c.client.Mail(m.FromEmail); err != nil {
		return fmt.Errorf("MAIL FROM refused: %w", err)
	}
	if err := c.client.Rcpt(m.ToEmail); err != nil {
		_ = c.client.Reset()
		return fmt.Errorf("RCPT TO refused: %w", err)
	}
	w, err := c.client.Data()
	if err != nil {
		_ = c.client.Reset()
		return fmt.Errorf("DATA refused: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("writing message body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message rejected: %w", err)
	}
	c.sent++
	return nil
}

// Sent reports how many messages went through this session.
func (c *Conn) Sent() int { return c.sent }

// Reset issues RSET, which doubles as a cheap keepalive between batches.
func (c *Conn) Reset() error {
	c.setDeadline()
	return c.client.Reset()
}

// Close ends the session politely, falling back to a hard close.
func (c *Conn) Close() error {
	c.setDeadline()
	if err := c.client.Quit(); err != nil {
		return c.client.Close()
	}
	return nil
}

func (c *Conn) setDeadline() {
	if c.raw != nil {
		_ = c.raw.SetDeadline(time.Now().Add(c.cfg.timeout()))
	}
}

// Probe describes what a relay answered during a connection test.
type Probe struct {
	OK         bool     `json:"ok"`
	TLS        bool     `json:"tls"`
	TLSVersion string   `json:"tlsVersion"`
	Cipher     string   `json:"cipher"`
	Auth       string   `json:"auth"`
	MaxSize    int64    `json:"maxSize"`
	Extensions []string `json:"extensions"`
	Latency    int64    `json:"latencyMs"`
	Error      string   `json:"error"`
}

var probedExtensions = []string{
	"STARTTLS", "AUTH", "SIZE", "8BITMIME", "SMTPUTF8",
	"PIPELINING", "CHUNKING", "DSN", "ENHANCEDSTATUSCODES",
}

// Test opens a full session (including authentication) and reports the server
// capabilities without sending anything.
func Test(cfg Config) Probe {
	start := time.Now()
	c, err := Dial(cfg)
	if err != nil {
		return Probe{Error: err.Error(), Latency: time.Since(start).Milliseconds()}
	}
	defer c.Close()

	p := Probe{OK: true, Latency: time.Since(start).Milliseconds()}
	if state, ok := c.client.TLSConnectionState(); ok {
		p.TLS = true
		p.TLSVersion = tlsVersionName(state.Version)
		p.Cipher = tls.CipherSuiteName(state.CipherSuite)
	}
	for _, ext := range probedExtensions {
		ok, params := c.client.Extension(ext)
		if !ok {
			continue
		}
		p.Extensions = append(p.Extensions, ext)
		switch ext {
		case "AUTH":
			p.Auth = params
		case "SIZE":
			if n, err := strconv.ParseInt(strings.TrimSpace(params), 10, 64); err == nil {
				p.MaxSize = n
			}
		}
	}
	return p
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	}
	return fmt.Sprintf("0x%04x", v)
}

// IsPermanent reports whether an SMTP error carries a 5xx reply code, meaning
// retrying the same recipient is pointless (bad mailbox, blocked sender...).
func IsPermanent(err error) bool {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return protoErr.Code >= 500 && protoErr.Code < 600
	}
	return false
}

// StatusCode extracts the SMTP reply code from an error, or 0 when the failure
// happened below the protocol (network, TLS, timeout).
func StatusCode(err error) int {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return protoErr.Code
	}
	return 0
}
