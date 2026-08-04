package mailer

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func parse(t *testing.T, m *Message) *mail.Message {
	t.Helper()
	raw, err := m.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("the produced message is not parseable: %v", err)
	}
	return msg
}

func TestBuildPlainOnly(t *testing.T) {
	msg := parse(t, &Message{
		FromEmail: "moi@exemple.fr", ToEmail: "toi@exemple.fr",
		Subject: "Salut", Text: "Bonjour",
	})
	ct := msg.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if msg.Header.Get("Message-ID") == "" {
		t.Error("Message-ID missing")
	}
}

func TestBuildAlternative(t *testing.T) {
	msg := parse(t, &Message{
		FromEmail: "moi@exemple.fr", ToEmail: "toi@exemple.fr",
		Subject: "Salut", Text: "Bonjour", HTML: "<p>Bonjour</p>",
	})
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mt != "multipart/alternative" {
		t.Fatalf("media type = %q", mt)
	}
	types := partTypes(t, msg.Body, params["boundary"])
	if len(types) != 2 || types[0] != "text/plain" || types[1] != "text/html" {
		t.Errorf("parts = %v, want [text/plain text/html]", types)
	}
}

func TestBuildWithAttachment(t *testing.T) {
	msg := parse(t, &Message{
		FromEmail: "moi@exemple.fr", ToEmail: "toi@exemple.fr",
		Subject: "Facture", HTML: "<p>Ci-joint</p>",
		Attachments: []Attachment{{Filename: "facture.pdf", MIMEType: "application/pdf", Content: []byte("%PDF-1.4 fake")}},
	})
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mt != "multipart/mixed" {
		t.Fatalf("media type = %q", mt)
	}
	types := partTypes(t, msg.Body, params["boundary"])
	if len(types) != 2 || types[0] != "text/html" || types[1] != "application/pdf" {
		t.Errorf("parts = %v", types)
	}
}

func TestBuildWithInlineImage(t *testing.T) {
	msg := parse(t, &Message{
		FromEmail: "moi@exemple.fr", ToEmail: "toi@exemple.fr",
		Subject: "Visuel", HTML: `<img src="cid:logo.blast">`,
		Attachments: []Attachment{{Filename: "logo.png", MIMEType: "image/png", Content: []byte("PNG"), Inline: true, ContentID: "logo.blast"}},
	})
	mt, _, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mt != "multipart/related" {
		t.Fatalf("media type = %q, want multipart/related", mt)
	}
}

func TestSubjectAndNamesAreEncoded(t *testing.T) {
	msg := parse(t, &Message{
		FromName: "Amélie Dupré", FromEmail: "moi@exemple.fr",
		ToName: "Bob", ToEmail: "toi@exemple.fr",
		// The euro sign is deliberate: three bytes in UTF-8, so it exercises the
		// encoded-word round trip beyond the two-byte accented letters.
		Subject: "Réunion à 14h, budget 1 200 € validé", Text: "x",
	})
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Réunion à 14h, budget 1 200 € validé" {
		t.Errorf("subject round-trip = %q", subject)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		t.Fatal(err)
	}
	if from.Name != "Amélie Dupré" {
		t.Errorf("from name = %q", from.Name)
	}
}

// A template variable holding a newline must not be able to inject headers.
func TestHeaderInjectionIsNeutralised(t *testing.T) {
	msg := parse(t, &Message{
		FromEmail: "moi@exemple.fr", ToEmail: "toi@exemple.fr",
		Subject: "Coucou", Text: "x",
		Headers: map[string]string{"X-Test": "valeur\r\nBcc: victime@exemple.fr"},
	})
	if bcc := msg.Header.Get("Bcc"); bcc != "" {
		t.Fatalf("header injection succeeded: Bcc = %q", bcc)
	}
	if got := msg.Header.Get("X-Test"); !strings.Contains(got, "Bcc:") {
		t.Errorf("X-Test = %q, the folded value should have stayed on one line", got)
	}
}

func TestBodyUsesCRLF(t *testing.T) {
	m := &Message{FromEmail: "a@b.fr", ToEmail: "c@d.fr", Subject: "s", Text: "ligne1\nligne2"}
	raw, err := m.Build()
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(strings.ReplaceAll(body, "\r\n", ""), "\n") {
		t.Error("the message contains a bare LF; SMTP requires CRLF line endings")
	}
}

func partTypes(t *testing.T, body io.Reader, boundary string) []string {
	t.Helper()
	mr := multipart.NewReader(body, boundary)
	var out []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("reading parts: %v", err)
		}
		mt, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("part media type: %v", err)
		}
		out = append(out, mt)
	}
}
