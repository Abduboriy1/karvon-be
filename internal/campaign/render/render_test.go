package render

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/campaign"
)

func TestAssembleKeepsSlotOrderRegardlessOfInput(t *testing.T) {
	slots := []Slot{
		{Type: campaign.ComponentPS, Body: "P.S. ps text ", ComponentID: uuid.New()},
		{Type: campaign.ComponentCTA, Body: "\ncta text\n", ComponentID: uuid.New()},
		{Type: campaign.ComponentHook, Body: "hook text", ComponentID: uuid.New()},
		{Type: campaign.ComponentSubject, Body: "  the subject ", ComponentID: uuid.New()},
		{Type: campaign.ComponentProblem, Body: "problem text", ComponentID: uuid.New()},
	}
	subject, body := Assemble(slots)
	if subject != "the subject" {
		t.Fatalf("subject = %q", subject)
	}
	want := "hook text\n\nproblem text\n\ncta text\n\nP.S. ps text"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}

	// A second permutation must give the same result.
	reversed := []Slot{slots[4], slots[3], slots[2], slots[1], slots[0]}
	s2, b2 := Assemble(reversed)
	if s2 != subject || b2 != body {
		t.Fatalf("reversed input assembled differently: %q / %q", s2, b2)
	}
}

func TestAssembleWithoutASubjectReturnsAnEmptySubject(t *testing.T) {
	subject, body := Assemble([]Slot{{Type: campaign.ComponentHook, Body: "hi"}})
	if subject != "" || body != "hi" {
		t.Fatalf("got %q / %q", subject, body)
	}
}

func TestRenderUsesTheFallbackWhenAVarIsEmpty(t *testing.T) {
	vars := map[string]string{"first_name": "", "company": "Acme"}
	cases := []struct{ name, template, want string }{
		{"empty var uses fallback", "Hi {{first_name|there}},", "Hi there,"},
		{"empty var with spaces", "Hi {{ first_name | there }},", "Hi there,"},
		{"non-empty var wins", "at {{company|your company}}", "at Acme"},
		{"empty var without fallback", "Hi {{first_name}},", "Hi ,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.template, vars); got != tc.want {
				t.Fatalf("Render(%q) = %q, want %q", tc.template, got, tc.want)
			}
		})
	}
}

func TestRenderRemovesUnknownPlaceholders(t *testing.T) {
	got := Render("Hello {{nobody}} and {{ nothing }}!", map[string]string{"first_name": "Ann"})
	if got != "Hello  and !" {
		t.Fatalf("got %q", got)
	}
	if got := Render("Hello {{nobody|friend}}!", nil); got != "Hello friend!" {
		t.Fatalf("unknown with fallback: got %q", got)
	}
}

func TestRenderIsCaseInsensitive(t *testing.T) {
	vars := map[string]string{"First_Name": "Ann", "company": "Acme"}
	got := Render("{{FIRST_NAME}} at {{Company}} / {{ first_name }}", vars)
	if got != "Ann at Acme / Ann" {
		t.Fatalf("got %q", got)
	}
}

func TestPlaceholdersAreDistinctAndOrdered(t *testing.T) {
	tpl := "Hi {{First_Name|there}}, {{company}} and {{ first_name }} again, {{Domain}}"
	got := Placeholders(tpl)
	want := []string{"first_name", "company", "domain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := Placeholders("no placeholders here"); len(got) != 0 {
		t.Fatalf("expected none, got %v", got)
	}
}

func TestContactVarsBuildsTheStandardMap(t *testing.T) {
	vars := ContactVars(ContactInput{FirstName: " Ann ", LastName: "Lee", Company: "Acme", Email: "ann@acme.test"})
	if vars["full_name"] != "Ann Lee" || vars["first_name"] != "Ann" || vars["company"] != "Acme" {
		t.Fatalf("unexpected vars: %v", vars)
	}
	for _, k := range []string{"first_name", "last_name", "full_name", "company", "title", "email",
		"domain", "phone", "website", "city", "state"} {
		if _, ok := vars[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
	if ContactVars(ContactInput{})["full_name"] != "" {
		t.Fatal("empty contact should have an empty full_name")
	}
}

func TestHTMLBodyEscapesAndWrapsParagraphs(t *testing.T) {
	text := "Hi <Ann> & co,\nline two\n\nSecond \"para\"\n\n\n\nThird"
	got := HTMLBody(text)
	want := "<p>Hi &lt;Ann&gt; &amp; co,<br/>line two</p>\n<p>Second &#34;para&#34;</p>\n<p>Third</p>"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if HTMLBody("") != "" || HTMLBody("  \n\n ") != "" {
		t.Fatal("empty input should give an empty body")
	}
}

func TestTextBodyRoundTripsHTMLBody(t *testing.T) {
	cases := []string{
		"Hi Ann,\n\nWe help <teams> ship & grow.\nSecond line.\n\nBest,\nBory",
		"Single paragraph only",
		"Quotes \"and\" 'apostrophes'",
	}
	for _, text := range cases {
		if got := TextBody(HTMLBody(text)); got != text {
			t.Fatalf("round trip of %q gave %q", text, got)
		}
	}
	if got := TextBody("<p>a<br>b</p><p>c</p>"); got != "a\nb\n\nc" {
		t.Fatalf("loose html: got %q", got)
	}
}

func TestPlaceholdersIsNeverNilForTextWithoutAny(t *testing.T) {
	// The result goes into a NOT NULL text[] column, so a body with no
	// placeholders must still produce an empty slice rather than nil.
	got := Placeholders("Quick question about your gym")
	if got == nil {
		t.Fatal("Placeholders returned nil for a body with no placeholders")
	}
	if len(got) != 0 {
		t.Fatalf("Placeholders returned %v, want an empty slice", got)
	}
}
