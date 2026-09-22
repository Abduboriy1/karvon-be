package business

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestCSVWriterWritesHeaderImmediately(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewCSVWriter(&buf, nil); err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(buf.String())
	want := strings.Join(CSVHeader, ",")
	if got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
}

func TestCSVWriterRowFormatting(t *testing.T) {
	var buf bytes.Buffer
	writer, err := NewCSVWriter(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}

	rating := 4.5
	reviews := int32(128)
	err = writer.Write(CSVRow{
		ID:           "01a0ba9a-cd53-7ad9-b4f8-7a01755aac06",
		Name:         `Iron "Works" Gym, Inc`,
		City:         "Austin",
		State:        "TX",
		Website:      "https://ironworksgym.com",
		Domain:       "ironworksgym.com",
		PrimaryEmail: "info@ironworksgym.com",
		EmailSource:  "mailto",
		AllEmails:    "info@ironworksgym.com;amy@ironworksgym.com",
		EmailsCount:  2,
		Rating:       &rating,
		Reviews:      &reviews,
		Suppressed:   false,
		FirstSeenAt:  time.Date(2026, 9, 19, 15, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (header + row)", len(records))
	}

	row := records[1]
	if len(row) != len(CSVHeader) {
		t.Fatalf("row has %d columns, header has %d", len(row), len(CSVHeader))
	}

	byName := map[string]string{}
	for i, column := range CSVHeader {
		byName[column] = row[i]
	}
	// Quoting and commas must survive the round trip untouched.
	if byName["name"] != `Iron "Works" Gym, Inc` {
		t.Errorf("name = %q", byName["name"])
	}
	if byName["rating"] != "4.5" {
		t.Errorf("rating = %q, want 4.5", byName["rating"])
	}
	if byName["reviews"] != "128" {
		t.Errorf("reviews = %q, want 128", byName["reviews"])
	}
	if byName["first_seen_at"] != "2026-09-19T15:04:05Z" {
		t.Errorf("first_seen_at = %q", byName["first_seen_at"])
	}
	if byName["suppressed"] != "false" {
		t.Errorf("suppressed = %q", byName["suppressed"])
	}
	if writer.Rows() != 1 {
		t.Errorf("Rows() = %d, want 1", writer.Rows())
	}
}

func TestCSVWriterLeavesOptionalNumbersEmpty(t *testing.T) {
	var buf bytes.Buffer
	writer, err := NewCSVWriter(&buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(CSVRow{ID: "id", Name: "No Ratings Gym"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	row := records[1]
	for i, column := range CSVHeader {
		if column == "rating" || column == "reviews" || column == "first_seen_at" {
			if row[i] != "" {
				t.Errorf("%s = %q, want empty", column, row[i])
			}
		}
	}
}

func TestCSVWriterFlushesPeriodically(t *testing.T) {
	var buf bytes.Buffer
	flushes := 0
	writer, err := NewCSVWriter(&buf, func() { flushes++ })
	if err != nil {
		t.Fatal(err)
	}
	// The header flush counts as one; each 200 rows adds another.
	for i := 0; i < 450; i++ {
		if err := writer.Write(CSVRow{ID: "id", Name: "Gym"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if flushes < 3 {
		t.Fatalf("flushes = %d, want at least 3 for 450 rows", flushes)
	}
}
