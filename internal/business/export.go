package business

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

// CSVHeader is the column order of every export this service produces.
var CSVHeader = []string{
	"id", "name", "category", "address", "city", "state", "zip", "phone",
	"website", "domain", "primary_email", "email_source", "all_emails",
	"emails_count", "rating", "reviews", "suppressed", "first_seen_at",
}

// CSVRow is the export-shaped view of a business, decoupled from the database row type.
type CSVRow struct {
	ID           string
	Name         string
	Category     string
	Address      string
	City         string
	State        string
	Zip          string
	Phone        string
	Website      string
	Domain       string
	PrimaryEmail string
	EmailSource  string
	AllEmails    string
	EmailsCount  int64
	Rating       *float64
	Reviews      *int32
	Suppressed   bool
	FirstSeenAt  time.Time
}

// CSVWriter streams rows to an io.Writer, flushing often enough that a slow client
// still receives data promptly while memory stays constant.
type CSVWriter struct {
	w       *csv.Writer
	flusher func()
	written int
}

// NewCSVWriter writes the header immediately so the browser starts the download.
// flush, when non-nil, is called every 200 rows (use http.Flusher.Flush).
func NewCSVWriter(dst io.Writer, flush func()) (*CSVWriter, error) {
	cw := &CSVWriter{w: csv.NewWriter(dst), flusher: flush}
	if err := cw.w.Write(CSVHeader); err != nil {
		return nil, err
	}
	cw.w.Flush()
	if flush != nil {
		flush()
	}
	return cw, cw.w.Error()
}

// Write appends one row.
func (c *CSVWriter) Write(r CSVRow) error {
	rating := ""
	if r.Rating != nil {
		rating = strconv.FormatFloat(*r.Rating, 'f', -1, 64)
	}
	reviews := ""
	if r.Reviews != nil {
		reviews = strconv.FormatInt(int64(*r.Reviews), 10)
	}
	firstSeen := ""
	if !r.FirstSeenAt.IsZero() {
		firstSeen = r.FirstSeenAt.UTC().Format(time.RFC3339)
	}

	err := c.w.Write([]string{
		r.ID, r.Name, r.Category, r.Address, r.City, r.State, r.Zip, r.Phone,
		r.Website, r.Domain, r.PrimaryEmail, r.EmailSource, r.AllEmails,
		strconv.FormatInt(r.EmailsCount, 10), rating, reviews,
		strconv.FormatBool(r.Suppressed), firstSeen,
	})
	if err != nil {
		return err
	}

	c.written++
	if c.written%200 == 0 {
		c.w.Flush()
		if c.flusher != nil {
			c.flusher()
		}
	}
	return c.w.Error()
}

// Close flushes any buffered rows.
func (c *CSVWriter) Close() error {
	c.w.Flush()
	if c.flusher != nil {
		c.flusher()
	}
	return c.w.Error()
}

// Rows reports how many data rows were written.
func (c *CSVWriter) Rows() int { return c.written }
