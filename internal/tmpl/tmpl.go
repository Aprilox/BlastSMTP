// Package tmpl renders the {{variable}} placeholders used in subjects and
// bodies. It is deliberately not text/template: the syntax has to stay readable
// for non-developers writing a campaign, and a broken placeholder must degrade
// into an empty string instead of aborting a send.
package tmpl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var placeholder = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// Context carries everything a placeholder can resolve against for one
// recipient.
type Context struct {
	// Fields holds the recipient columns, keyed by normalised header name.
	Fields map[string]string
	// Index is the 1-based position of the recipient in the campaign.
	Index int
	// Total is the number of recipients in the campaign.
	Total int
	// IndexStart shifts {{index}} so counters can begin at any value.
	IndexStart int
	// Now is the reference time for date placeholders.
	Now time.Time

	rng *mathrand.Rand
}

// NewContext builds a context for one recipient. The random generator is seeded
// from seed+index so a preview shows exactly what the recipient will receive.
func NewContext(fields map[string]string, index, total, indexStart int, seed int64) *Context {
	c := &Context{
		Fields:     normalizeFields(fields),
		Index:      index,
		Total:      total,
		IndexStart: indexStart,
		Now:        time.Now(),
		rng:        mathrand.New(mathrand.NewSource(seed + int64(index)*7919)),
	}
	return c
}

func normalizeFields(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields)+2)
	for k, v := range fields {
		out[NormalizeKey(k)] = v
	}
	// Derived conveniences: {{emailuser}} and {{emaildomain}} always work as
	// soon as an email column exists.
	if email := out["email"]; email != "" {
		if i := strings.LastIndex(email, "@"); i > 0 {
			out["emailuser"] = email[:i]
			out["emaildomain"] = email[i+1:]
		}
	}
	return out
}

// NormalizeKey turns a CSV header into the identifier used inside {{ }}.
// Diacritics are folded so a "Prénom" column answers to {{prenom}}: nobody
// wants to type accents inside a placeholder.
func NormalizeKey(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if folded, ok := diacritics[r]; ok {
			b.WriteString(folded)
			continue
		}
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ' || r == '.':
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// diacritics covers the Latin-1 range, which is everything a French, Spanish,
// German or Portuguese export will contain. Folding this by hand avoids
// pulling golang.org/x/text in for a dozen runes.
var diacritics = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'æ': "ae",
	'ç': "c",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ñ': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'œ': "oe",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u",
	'ý': "y", 'ÿ': "y",
	'ß': "ss",
}

// Render expands every placeholder in s. Unknown names resolve to an empty
// string and are reported in missing so the UI can warn before sending.
func Render(s string, ctx *Context) (out string, missing []string) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	seen := map[string]bool{}
	out = placeholder.ReplaceAllStringFunc(s, func(match string) string {
		expr := strings.TrimSpace(match[2 : len(match)-2])
		value, ok := ctx.resolve(expr)
		if !ok && !seen[expr] {
			seen[expr] = true
			missing = append(missing, expr)
		}
		return value
	})
	return out, missing
}

// Placeholders lists the distinct expressions used in s, in order of first use.
func Placeholders(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		expr := strings.TrimSpace(m[1])
		if !seen[expr] {
			seen[expr] = true
			out = append(out, expr)
		}
	}
	return out
}

// resolve evaluates one expression. The grammar is:
//
//	name                 recipient column or built-in
//	name|fallback        same, with a value used when empty
//	fn:arg               built-in taking an argument
//	fn:arg|fallback      both
func (c *Context) resolve(expr string) (string, bool) {
	fallback := ""
	if i := strings.Index(expr, "|"); i >= 0 {
		fallback = strings.TrimSpace(expr[i+1:])
		expr = strings.TrimSpace(expr[:i])
	}

	name, arg := expr, ""
	if i := strings.Index(expr, ":"); i >= 0 {
		name = strings.TrimSpace(expr[:i])
		arg = strings.TrimSpace(expr[i+1:])
	}
	name = strings.ToLower(name)

	value, ok := c.builtin(name, arg)
	if !ok {
		value, ok = c.Fields[NormalizeKey(expr)]
	}
	if strings.TrimSpace(value) == "" && fallback != "" {
		return fallback, true
	}
	return value, ok
}

func (c *Context) builtin(name, arg string) (string, bool) {
	switch name {
	case "index", "i", "n", "num":
		start := c.IndexStart
		if arg != "" {
			if v, err := strconv.Atoi(arg); err == nil {
				start = v
			}
		}
		if start == 0 {
			start = 1
		}
		return strconv.Itoa(start + c.Index - 1), true

	case "count", "total":
		return strconv.Itoa(c.Total), true

	case "date":
		return c.Now.Format(goLayout(arg, "02/01/2006")), true
	case "time":
		return c.Now.Format(goLayout(arg, "15:04")), true
	case "datetime":
		return c.Now.Format(goLayout(arg, "02/01/2006 15:04")), true
	case "year":
		return strconv.Itoa(c.Now.Year()), true
	case "day":
		return c.Now.Format("02"), true
	case "month":
		return c.Now.Format("01"), true
	case "timestamp":
		return strconv.FormatInt(c.Now.Unix(), 10), true

	case "rand", "random":
		lo, hi := parseRange(arg, 1000, 9999)
		if hi <= lo {
			return strconv.Itoa(lo), true
		}
		return strconv.Itoa(lo + c.rng.Intn(hi-lo+1)), true

	case "randstr":
		return c.randomString(atoiOr(arg, 8), "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"), true
	case "randnum":
		return c.randomString(atoiOr(arg, 6), "0123456789"), true
	case "randhex":
		return c.randomString(atoiOr(arg, 8), "0123456789abcdef"), true

	case "uuid":
		return c.uuid(), true

	case "spin", "choice":
		// Options are separated by ';' because '|' already means "fallback".
		options := splitClean(arg, ";")
		if len(options) == 0 {
			return "", true
		}
		return options[c.rng.Intn(len(options))], true

	case "upper":
		return strings.ToUpper(c.field(arg)), true
	case "lower":
		return strings.ToLower(c.field(arg)), true
	case "capitalize", "title":
		return capitalizeWords(c.field(arg)), true
	case "trim":
		return strings.TrimSpace(c.field(arg)), true
	}
	return "", false
}

// field resolves an argument that may be either a column name or a literal.
func (c *Context) field(arg string) string {
	if v, ok := c.Fields[NormalizeKey(arg)]; ok {
		return v
	}
	return arg
}

func (c *Context) randomString(n int, alphabet string) string {
	if n <= 0 {
		n = 8
	}
	if n > 512 {
		n = 512
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[c.rng.Intn(len(alphabet))]
	}
	return string(b)
}

func (c *Context) uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		for i := range b {
			b[i] = byte(c.rng.Intn(256))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// goLayout translates a human date pattern (DD/MM/YYYY) into a Go layout.
// A pattern already written as a Go layout is passed through untouched.
func goLayout(pattern, def string) string {
	if pattern == "" {
		return def
	}
	if strings.ContainsAny(pattern, "0123456789") {
		return pattern // already a Go reference layout such as 2006-01-02
	}
	r := strings.NewReplacer(
		"YYYY", "2006", "YY", "06",
		"MMMM", "January", "MMM", "Jan", "MM", "01",
		"DDDD", "Monday", "DDD", "Mon", "DD", "02",
		"HH", "15", "hh", "03", "mm", "04", "ss", "05", "A", "PM",
	)
	return r.Replace(pattern)
}

func parseRange(arg string, defLo, defHi int) (int, int) {
	parts := strings.SplitN(arg, "-", 2)
	if len(parts) != 2 {
		if n, err := strconv.Atoi(strings.TrimSpace(arg)); err == nil {
			return 0, n
		}
		return defLo, defHi
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return defLo, defHi
	}
	return lo, hi
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func splitClean(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func capitalizeWords(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	startOfWord := true
	for _, r := range s {
		if startOfWord {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		startOfWord = !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}
	return b.String()
}
