package recipients

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// The files under examples/ are shipped in the repository and are the first
// thing a new user imports. An editor silently re-saving one of them as
// Windows-1252 would break accents for everyone, so guard them here.
func TestShippedExamplesAreValidUTF8(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("examples directory unavailable: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.Valid(data) {
			t.Errorf("%s is not valid UTF-8; it was probably re-saved with an ANSI encoding", e.Name())
		}
	}
}

func TestShippedContactsCSVParses(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "contacts.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("example unavailable: %v", err)
	}
	res, err := Parse("contacts.csv", data)
	if err != nil {
		t.Fatalf("the shipped example does not parse: %v", err)
	}
	if len(res.Recipients) != 5 {
		t.Errorf("got %d recipients, want 5", len(res.Recipients))
	}
	if len(res.Rejected) != 0 {
		t.Errorf("unexpected rejections: %v", res.Rejected)
	}

	// The accented headers must fold to the names the README documents.
	want := []string{"prenom", "nom", "email", "societe", "ville", "offre"}
	if len(res.Columns) != len(want) {
		t.Fatalf("columns = %v, want %v", res.Columns, want)
	}
	for i, w := range want {
		if res.Columns[i] != w {
			t.Errorf("column %d = %q, want %q", i, res.Columns[i], w)
		}
	}

	// And the accented values must survive intact.
	first := res.Recipients[0]
	if first.Fields["prenom"] != "Amélie" || first.Fields["societe"] != "ACME" {
		t.Errorf("first row = %v", first.Fields)
	}
	if last := res.Recipients[4]; last.Fields["prenom"] != "Élodie" {
		t.Errorf("last row prenom = %q, want %q", last.Fields["prenom"], "Élodie")
	}
}
