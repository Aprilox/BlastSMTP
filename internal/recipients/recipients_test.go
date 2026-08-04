package recipients

import "testing"

func TestParseCSVWithHeader(t *testing.T) {
	in := []byte("Prénom;Email;Société\nAmélie;amelie@exemple.fr;ACME\nBob;bob@exemple.fr;Globex\n")
	res, err := Parse("clients.csv", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recipients) != 2 {
		t.Fatalf("got %d recipients", len(res.Recipients))
	}
	if !res.HasHeader || res.Delimiter != ";" {
		t.Errorf("header=%v delimiter=%q", res.HasHeader, res.Delimiter)
	}
	r := res.Recipients[0]
	if r.Email != "amelie@exemple.fr" {
		t.Errorf("email = %q", r.Email)
	}
	if r.Fields["prenom"] != "Amélie" || r.Fields["societe"] != "ACME" {
		t.Errorf("fields = %v", r.Fields)
	}
}

func TestParseCSVWithoutHeader(t *testing.T) {
	in := []byte("amelie@exemple.fr,Amélie\nbob@exemple.fr,Bob\n")
	res, err := Parse("liste.csv", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.HasHeader {
		t.Error("a headerless file was reported as having headers")
	}
	if len(res.Recipients) != 2 {
		t.Fatalf("got %d recipients", len(res.Recipients))
	}
}

func TestEmailColumnIsExposedAsEmail(t *testing.T) {
	// Whatever the column is called, {{email}} must resolve.
	in := []byte("nom,courriel\nAmélie,amelie@exemple.fr\n")
	res, err := Parse("x.csv", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Recipients[0].Fields["email"] != "amelie@exemple.fr" {
		t.Errorf("fields = %v", res.Recipients[0].Fields)
	}
}

func TestDuplicatesAndRejects(t *testing.T) {
	in := []byte("email\na@exemple.fr\nA@Exemple.fr\npas-une-adresse\nb@exemple\n")
	res, err := Parse("x.csv", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recipients) != 1 {
		t.Errorf("got %d recipients, want 1", len(res.Recipients))
	}
	if res.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", res.Duplicates)
	}
	if len(res.Rejected) != 2 {
		t.Errorf("rejected = %v, want 2", res.Rejected)
	}
}

func TestParsePlainText(t *testing.T) {
	in := []byte("# commentaire\namelie@exemple.fr\nBob Martin <bob@exemple.fr>\n\n")
	res, err := Parse("liste.txt", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recipients) != 2 {
		t.Fatalf("got %d recipients", len(res.Recipients))
	}
	if res.Recipients[1].Name != "Bob Martin" {
		t.Errorf("name = %q", res.Recipients[1].Name)
	}
}

func TestBOMIsStripped(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("email;nom\na@exemple.fr;A\n")...)
	res, err := Parse("x.csv", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Columns[0] != "email" {
		t.Errorf("columns = %v", res.Columns)
	}
}

func TestTabDelimited(t *testing.T) {
	in := []byte("email\tville\na@exemple.fr\tLyon\n")
	res, err := Parse("x.tsv", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Recipients[0].Fields["ville"] != "Lyon" {
		t.Errorf("fields = %v", res.Recipients[0].Fields)
	}
}
