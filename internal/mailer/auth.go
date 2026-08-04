package mailer

import (
	"errors"
	"fmt"
	"net/smtp"
	"strings"
)

// loginAuth implements the non-standard AUTH LOGIN mechanism. It is not part of
// the standard library because it never made it into an RFC, yet Office 365,
// Exchange and most cPanel relays refuse AUTH PLAIN and only offer this one.
type loginAuth struct {
	username string
	password string
	host     string
	insecure bool
	step     int
}

// LoginAuth returns an smtp.Auth implementing AUTH LOGIN. When insecure is
// false the credentials are only sent over a TLS-protected connection.
func LoginAuth(username, password, host string, insecure bool) smtp.Auth {
	return &loginAuth{username: username, password: password, host: host, insecure: insecure}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !a.insecure {
		return "", nil, errors.New("AUTH LOGIN refused on an unencrypted connection (enable TLS or allow cleartext auth)")
	}
	if !a.insecure && server.Name != a.host {
		return "", nil, fmt.Errorf("unexpected server name %q, wanted %q", server.Name, a.host)
	}
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	// Servers are inconsistent about the prompt they send ("Username:",
	// "User Name", or even an empty challenge), so match on the text when we
	// recognise it and fall back to the plain request order otherwise.
	switch normalizePrompt(fromServer) {
	case "username", "user name", "user":
		a.step = 1
		return []byte(a.username), nil
	case "password", "pass":
		a.step = 2
		return []byte(a.password), nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected AUTH LOGIN challenge: %q", string(fromServer))
}

func normalizePrompt(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.TrimSuffix(s, ":")
	return strings.ToLower(strings.TrimSpace(s))
}

// plainAuth mirrors smtp.PlainAuth but can be told to proceed on an
// unencrypted connection, which the standard library flatly refuses. Only used
// when the operator explicitly opts in (local relays, test servers).
type plainAuth struct {
	username string
	password string
	host     string
	insecure bool
}

// PlainAuth returns an smtp.Auth implementing AUTH PLAIN. Set insecure to allow
// sending credentials over a connection without TLS.
func PlainAuth(username, password, host string, insecure bool) smtp.Auth {
	if !insecure {
		return smtp.PlainAuth("", username, password, host)
	}
	return &plainAuth{username: username, password: password, host: host, insecure: true}
}

func (a *plainAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	resp := []byte("\x00" + a.username + "\x00" + a.password)
	return "PLAIN", resp, nil
}

func (a *plainAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge during AUTH PLAIN")
	}
	return nil, nil
}
