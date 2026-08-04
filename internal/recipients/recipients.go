// Package recipients parses the address lists a campaign is sent to. It accepts
// CSV, TSV and plain text and tries hard to make sense of real-world exports
// rather than demanding one canonical format.
package recipients

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net/mail"
	"regexp"
	"strings"

	"github.com/aprilox/blastsmtp/internal/tmpl"
)

// Recipient is one deliverable address plus every column that came with it.
type Recipient struct {
	Email  string            `json:"email"`
	Name   string            `json:"name"`
	Fields map[string]string `json:"fields"`
	Line   int               `json:"line"`
}

// Rejected records a line that could not be used, with the reason why.
type Rejected struct {
	Line   int    `json:"line"`
	Raw    string `json:"raw"`
	Reason string `json:"reason"`
}

// Result is the outcome of parsing one file.
type Result struct {
	Recipients []Recipient `json:"recipients"`
	Columns    []string    `json:"columns"`
	Rejected   []Rejected  `json:"rejected"`
	Duplicates int         `json:"duplicates"`
	Delimiter  string      `json:"delimiter"`
	HasHeader  bool        `json:"hasHeader"`
	Format     string      `json:"format"`
}

// emailPattern is intentionally permissive: net/mail does the real validation,
// this is only used to pull an address out of a noisy line.
var emailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,63}`)

// Column names that identify the address, in order of preference. Covers the
// usual French and English exports.
var emailHeaders = []string{
	"email", "e_mail", "mail", "courriel", "adresse_email", "adresse_mail",
	"email_address", "address", "adresse", "to", "destinataire", "emails",
}

// Column names that identify a display name, in order of preference.
var nameHeaders = []string{
	"name", "nom", "fullname", "full_name", "nom_complet", "contact",
	"firstname", "first_name", "prenom", "prénom", "civilite", "société", "societe", "company", "entreprise",
}

// Parse reads a recipient list. The filename is only used as a hint; the
// content decides how it is actually parsed.
func Parse(filename string, data []byte) (*Result, error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, fmt.Errorf("the file is empty")
	}

	delim, ok := detectDelimiter(text)
	if !ok {
		return parsePlain(text)
	}
	return parseCSV(text, delim)
}

func detectDelimiter(text string) (rune, bool) {
	firstLine := text
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		firstLine = text[:i]
	}
	best, bestCount := rune(0), 0
	for _, d := range []rune{',', ';', '\t', '|'} {
		if n := strings.Count(firstLine, string(d)); n > bestCount {
			best, bestCount = d, n
		}
	}
	return best, bestCount > 0
}

func parseCSV(text string, delim rune) (*Result, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = delim
	r.FieldsPerRecord = -1 // rows with a missing trailing column are common
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("unreadable CSV: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("the file contains no rows")
	}

	res := &Result{Delimiter: string(delim), Format: "csv"}

	// A header row is one whose cells contain no address at all.
	header := rows[0]
	res.HasHeader = !rowHasEmail(header)
	var columns []string
	if res.HasHeader {
		for i, h := range header {
			key := tmpl.NormalizeKey(h)
			if key == "" {
				key = fmt.Sprintf("col%d", i+1)
			}
			columns = append(columns, uniqueKey(columns, key))
		}
		rows = rows[1:]
	} else {
		for i := range header {
			columns = append(columns, fmt.Sprintf("col%d", i+1))
		}
	}

	emailCol := pickColumn(columns, emailHeaders)
	if emailCol < 0 {
		emailCol = guessEmailColumn(rows, len(columns))
	}
	if emailCol < 0 {
		return nil, fmt.Errorf("no column containing email addresses was found")
	}
	// The address column is always exposed as {{email}}, whatever it is called.
	columns[emailCol] = "email"
	nameCol := pickColumn(columns, nameHeaders)

	res.Columns = columns
	lineOffset := 1
	if res.HasHeader {
		lineOffset = 2
	}

	seen := make(map[string]bool, len(rows))
	for i, row := range rows {
		line := i + lineOffset
		if isBlankRow(row) {
			continue
		}
		raw := strings.Join(row, string(delim))
		if emailCol >= len(row) {
			res.Rejected = append(res.Rejected, Rejected{line, raw, "missing email column"})
			continue
		}
		addr, name, err := parseAddress(row[emailCol])
		if err != nil {
			res.Rejected = append(res.Rejected, Rejected{line, raw, err.Error()})
			continue
		}
		key := strings.ToLower(addr)
		if seen[key] {
			res.Duplicates++
			continue
		}
		seen[key] = true

		fields := make(map[string]string, len(columns))
		for j, col := range columns {
			if j < len(row) {
				fields[col] = strings.TrimSpace(row[j])
			} else {
				fields[col] = ""
			}
		}
		fields["email"] = addr
		if name == "" && nameCol >= 0 && nameCol < len(row) {
			name = strings.TrimSpace(row[nameCol])
		}
		if _, exists := fields["name"]; !exists && name != "" {
			fields["name"] = name
		}
		res.Recipients = append(res.Recipients, Recipient{Email: addr, Name: name, Fields: fields, Line: line})
	}
	return res, nil
}

func parsePlain(text string) (*Result, error) {
	res := &Result{Columns: []string{"email", "name"}, Format: "txt"}
	seen := map[string]bool{}
	first := true
	for i, raw := range strings.Split(text, "\n") {
		line := i + 1
		trimmed := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A single-column CSV export has no delimiter to detect, so it lands
		// here with its header still attached. Drop it instead of reporting
		// "email" as a malformed address.
		if first {
			first = false
			if isHeaderWord(trimmed) {
				res.HasHeader = true
				continue
			}
		}
		addr, name, err := parseAddress(trimmed)
		if err != nil {
			res.Rejected = append(res.Rejected, Rejected{line, trimmed, err.Error()})
			continue
		}
		key := strings.ToLower(addr)
		if seen[key] {
			res.Duplicates++
			continue
		}
		seen[key] = true
		res.Recipients = append(res.Recipients, Recipient{
			Email:  addr,
			Name:   name,
			Fields: map[string]string{"email": addr, "name": name},
			Line:   line,
		})
	}
	if len(res.Recipients) == 0 {
		return res, fmt.Errorf("no valid email address found in the file")
	}
	return res, nil
}

// parseAddress accepts "user@host", "Name <user@host>" and anything with an
// address embedded in it.
func parseAddress(s string) (email, name string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("empty value")
	}
	if a, e := mail.ParseAddress(s); e == nil {
		if err := validate(a.Address); err != nil {
			return "", "", err
		}
		return a.Address, a.Name, nil
	}
	if m := emailPattern.FindString(s); m != "" {
		if err := validate(m); err != nil {
			return "", "", err
		}
		// Whatever precedes the address is treated as the display name.
		lead := strings.TrimSpace(strings.Trim(strings.Split(s, m)[0], "<>\"',;:"))
		return m, lead, nil
	}
	return "", "", fmt.Errorf("not an email address")
}

func validate(addr string) error {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return fmt.Errorf("malformed address")
	}
	domain := addr[at+1:]
	if !strings.Contains(domain, ".") || strings.HasSuffix(domain, ".") {
		return fmt.Errorf("invalid domain %q", domain)
	}
	if strings.ContainsAny(addr, " \t\r\n,;") {
		return fmt.Errorf("address contains a forbidden character")
	}
	return nil
}

// isHeaderWord reports whether a lone line is a column title rather than data.
func isHeaderWord(s string) bool {
	key := tmpl.NormalizeKey(s)
	for _, h := range emailHeaders {
		if key == h {
			return true
		}
	}
	return false
}

func rowHasEmail(row []string) bool {
	for _, cell := range row {
		if _, _, err := parseAddress(cell); err == nil {
			return true
		}
	}
	return false
}

func isBlankRow(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func pickColumn(columns []string, candidates []string) int {
	for _, want := range candidates {
		for i, col := range columns {
			if col == want {
				return i
			}
		}
	}
	// Fall back to a partial match ("email_pro", "nom_client"...).
	for _, want := range candidates {
		for i, col := range columns {
			if strings.Contains(col, want) {
				return i
			}
		}
	}
	return -1
}

// guessEmailColumn picks the column where most values look like addresses.
func guessEmailColumn(rows [][]string, width int) int {
	best, bestHits := -1, 0
	sample := min(len(rows), 50)
	for col := 0; col < width; col++ {
		hits := 0
		for _, row := range rows[:sample] {
			if col < len(row) {
				if _, _, err := parseAddress(row[col]); err == nil {
					hits++
				}
			}
		}
		if hits > bestHits {
			best, bestHits = col, hits
		}
	}
	if bestHits == 0 {
		return -1
	}
	return best
}

func uniqueKey(existing []string, key string) string {
	candidate := key
	for n := 2; ; n++ {
		clash := false
		for _, e := range existing {
			if e == candidate {
				clash = true
				break
			}
		}
		if !clash {
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", key, n)
	}
}

// WriteReport dumps the per-recipient outcome of a campaign as CSV.
func WriteReport(w io.Writer, header []string, rows [][]string) error {
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil { // Excel needs the BOM
		return err
	}
	cw := csv.NewWriter(w)
	cw.Comma = ';' // French Excel default
	if err := cw.Write(header); err != nil {
		return err
	}
	if err := cw.WriteAll(rows); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}
