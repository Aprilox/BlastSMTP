package tmpl

import (
	"strings"
	"testing"
	"time"
)

func ctx(fields map[string]string, index, total int) *Context {
	c := NewContext(fields, index, total, 1, 42)
	c.Now = time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)
	return c
}

func TestRenderFields(t *testing.T) {
	c := ctx(map[string]string{"Prénom": "amélie", "email": "a@exemple.fr", "Ville": ""}, 3, 10)

	tests := []struct {
		in   string
		want string
	}{
		{"Bonjour {{prenom}}", "Bonjour amélie"},
		{"{{capitalize:prenom}}", "Amélie"},
		{"{{upper:prenom}}", "AMÉLIE"},
		{"{{ville|Paris}}", "Paris"},     // empty column falls back
		{"{{inconnu|Client}}", "Client"}, // unknown name falls back
		{"{{emailuser}}@{{emaildomain}}", "a@exemple.fr"},
		{"n°{{index}}", "n°3"},
		{"n°{{index:1000}}", "n°1002"},
		{"{{count}} contacts", "10 contacts"},
		{"{{date}}", "04/08/2026"},
		{"{{date:YYYY-MM-DD}}", "2026-08-04"},
		{"{{time}}", "14:30"},
		{"{{year}}", "2026"},
		{"sans variable", "sans variable"},
	}
	for _, tc := range tests {
		got, _ := Render(tc.in, c)
		if got != tc.want {
			t.Errorf("Render(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderReportsMissing(t *testing.T) {
	c := ctx(map[string]string{"email": "a@b.fr"}, 1, 1)
	got, missing := Render("Salut {{prenom}} de {{societe}}", c)
	if got != "Salut  de " {
		t.Errorf("got %q", got)
	}
	if len(missing) != 2 {
		t.Fatalf("missing = %v, want 2 entries", missing)
	}
}

func TestRandomIsStablePerRecipient(t *testing.T) {
	// The preview must show exactly what the recipient will receive, so the
	// same seed and index have to produce the same draw.
	a, _ := Render("{{rand:1-1000000}}-{{randstr:10}}", ctx(nil, 7, 100))
	b, _ := Render("{{rand:1-1000000}}-{{randstr:10}}", ctx(nil, 7, 100))
	if a != b {
		t.Errorf("same index gave %q then %q", a, b)
	}
	c, _ := Render("{{rand:1-1000000}}-{{randstr:10}}", ctx(nil, 8, 100))
	if a == c {
		t.Errorf("index 7 and 8 both gave %q", a)
	}
}

func TestSpinPicksOneOption(t *testing.T) {
	for i := 1; i <= 30; i++ {
		got, _ := Render("{{spin:Bonjour;Salut;Coucou}}", ctx(nil, i, 30))
		switch got {
		case "Bonjour", "Salut", "Coucou":
		default:
			t.Fatalf("spin returned %q", got)
		}
	}
}

func TestRandStrLength(t *testing.T) {
	got, _ := Render("{{randstr:16}}", ctx(nil, 1, 1))
	if len(got) != 16 {
		t.Errorf("len = %d, want 16", len(got))
	}
}

func TestUUIDShape(t *testing.T) {
	got, _ := Render("{{uuid}}", ctx(nil, 1, 1))
	if len(got) != 36 || strings.Count(got, "-") != 4 {
		t.Errorf("uuid = %q", got)
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := map[string]string{
		"Prénom":         "prenom",
		"Société":        "societe",
		"E-Mail":         "e_mail",
		" Nom Complet ":  "nom_complet",
		"Chiffre d'aff.": "chiffre_daff",
	}
	for in, want := range tests {
		if got := NormalizeKey(in); got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	got := Placeholders("{{a}} {{b|x}} {{a}}")
	if len(got) != 2 || got[0] != "a" || got[1] != "b|x" {
		t.Errorf("Placeholders = %v", got)
	}
}
