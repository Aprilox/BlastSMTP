package mailer

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"
)

// UserAgent is advertised in the X-Mailer header of every outgoing message.
var UserAgent = "BlastSMTP"

// Attachment is a file carried by a message. Inline attachments are referenced
// from the HTML body through cid:ContentID.
type Attachment struct {
	Filename  string `json:"filename"`
	MIMEType  string `json:"mimeType"`
	Content   []byte `json:"-"`
	Inline    bool   `json:"inline"`
	ContentID string `json:"contentId"`
}

// Message is a single rendered email, ready to be handed to an SMTP session.
type Message struct {
	FromName    string
	FromEmail   string
	ToName      string
	ToEmail     string
	ReplyTo     string
	Subject     string
	HTML        string
	Text        string
	Attachments []Attachment
	Headers     map[string]string
	Date        time.Time
}

type part struct {
	header textproto.MIMEHeader
	body   []byte
}

// Build renders the message as an RFC 5322 byte stream. The body is laid out as
// the smallest MIME tree that fits the content:
//
//	multipart/mixed          (only when regular attachments are present)
//	  multipart/related      (only when inline images are present)
//	    multipart/alternative(only when both text and HTML bodies are present)
//	      text/plain
//	      text/html
func (m *Message) Build() ([]byte, error) {
	body, err := m.buildBody()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}

	from := mail.Address{Name: m.FromName, Address: m.FromEmail}
	to := mail.Address{Name: m.ToName, Address: m.ToEmail}

	writeHeader(&buf, "Date", date.Format(time.RFC1123Z))
	writeHeader(&buf, "From", from.String())
	writeHeader(&buf, "To", to.String())
	if m.ReplyTo != "" {
		writeHeader(&buf, "Reply-To", m.ReplyTo)
	}
	writeHeader(&buf, "Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	writeHeader(&buf, "Message-ID", m.messageID())
	writeHeader(&buf, "MIME-Version", "1.0")
	writeHeader(&buf, "X-Mailer", UserAgent)

	// Custom headers come last so an operator can override anything above
	// (List-Unsubscribe, X-Priority, custom tracking headers...).
	for _, k := range sortedKeys(m.Headers) {
		v := strings.TrimSpace(m.Headers[k])
		if v == "" {
			continue
		}
		writeHeader(&buf, k, v)
	}
	for _, k := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		if v := body.header.Get(k); v != "" {
			writeHeader(&buf, k, v)
		}
	}
	buf.WriteString("\r\n")
	buf.Write(body.body)
	return buf.Bytes(), nil
}

func (m *Message) buildBody() (part, error) {
	var inline, regular []Attachment
	for _, a := range m.Attachments {
		if a.Inline {
			inline = append(inline, a)
		} else {
			regular = append(regular, a)
		}
	}

	content, err := m.contentPart()
	if err != nil {
		return part{}, err
	}

	if len(inline) > 0 {
		parts := []part{content}
		for _, a := range inline {
			parts = append(parts, attachmentPart(a))
		}
		content, err = multipartOf("related", parts, map[string]string{"type": mediaType(content)})
		if err != nil {
			return part{}, err
		}
	}

	if len(regular) > 0 {
		parts := []part{content}
		for _, a := range regular {
			parts = append(parts, attachmentPart(a))
		}
		content, err = multipartOf("mixed", parts, nil)
		if err != nil {
			return part{}, err
		}
	}
	return content, nil
}

func (m *Message) contentPart() (part, error) {
	html := strings.TrimSpace(m.HTML)
	text := strings.TrimSpace(m.Text)
	switch {
	case html != "" && text != "":
		return multipartOf("alternative", []part{
			textPart("text/plain", m.Text),
			textPart("text/html", m.HTML),
		}, nil)
	case html != "":
		return textPart("text/html", m.HTML), nil
	default:
		return textPart("text/plain", m.Text), nil
	}
}

func (m *Message) messageID() string {
	domain := "localhost"
	if i := strings.LastIndex(m.FromEmail, "@"); i >= 0 && i+1 < len(m.FromEmail) {
		domain = m.FromEmail[i+1:]
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("<%s@%s>", hex.EncodeToString(b[:]), domain)
}

func textPart(mimeType, content string) part {
	var b bytes.Buffer
	qw := quotedprintable.NewWriter(&b)
	_, _ = qw.Write([]byte(normalizeCRLF(content)))
	_ = qw.Close()

	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mimeType+"; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	return part{header: h, body: b.Bytes()}
}

func attachmentPart(a Attachment) part {
	ct := a.MIMEType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType(ct, map[string]string{"name": a.Filename}))
	h.Set("Content-Transfer-Encoding", "base64")

	disposition := "attachment"
	if a.Inline {
		disposition = "inline"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}))
	if a.Inline && a.ContentID != "" {
		h.Set("Content-ID", "<"+a.ContentID+">")
	}
	return part{header: h, body: base64Wrap(a.Content)}
}

func multipartOf(subtype string, parts []part, extraParams map[string]string) (part, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		pw, err := mw.CreatePart(p.header)
		if err != nil {
			return part{}, err
		}
		if _, err := pw.Write(p.body); err != nil {
			return part{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return part{}, err
	}

	params := map[string]string{"boundary": mw.Boundary()}
	for k, v := range extraParams {
		if v != "" {
			params[k] = v
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType("multipart/"+subtype, params))
	return part{header: h, body: b.Bytes()}, nil
}

func mediaType(p part) string {
	mt, _, err := mime.ParseMediaType(p.header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return mt
}

func base64Wrap(data []byte) []byte {
	const lineLen = 76
	enc := base64.StdEncoding.EncodeToString(data)
	var b bytes.Buffer
	b.Grow(len(enc) + 2*(len(enc)/lineLen+1))
	for i := 0; i < len(enc); i += lineLen {
		end := min(i+lineLen, len(enc))
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

func writeHeader(buf *bytes.Buffer, key, value string) {
	// Header values must never carry a bare CR/LF: that is how header
	// injection through a template variable would happen.
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	fmt.Fprintf(buf, "%s: %s\r\n", textproto.CanonicalMIMEHeaderKey(key), value)
}

func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
