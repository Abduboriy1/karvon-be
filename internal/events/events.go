// Package events persists job progress into job_events and fans it out to SSE
// subscribers through Postgres LISTEN/NOTIFY, so any API replica can serve any stream.
package events

import "time"

// Channel is the Postgres NOTIFY channel carrying job ids.
const Channel = "job_events"

// Type enumerates the event kinds the frontend understands.
type Type string

// The event kinds the SSE stream emits.
const (
	TypeProgress Type = "progress"
	TypeLog      Type = "log"
	TypeStatus   Type = "status"
)

// Level is the severity of a log event.
type Level string

// The severities a log event can carry.
const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Progress is the payload of a progress event.
type Progress struct {
	QueriesDone   int   `json:"queries_done"`
	QueriesTotal  int   `json:"queries_total"`
	ListingsFound int   `json:"listings_found"`
	SitesCrawled  int   `json:"sites_crawled"`
	SitesTotal    int   `json:"sites_total"`
	EmailsFound   int   `json:"emails_found"`
	CostCents     int64 `json:"cost_cents"`
}

// Log is the payload of a log event.
type Log struct {
	Level Level     `json:"level"`
	Msg   string    `json:"msg"`
	TS    time.Time `json:"ts"`
}

// Status is the payload of a status event.
type Status struct {
	Status     string     `json:"status"`
	Error      *string    `json:"error,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// IsTerminal reports whether a status ends the stream.
func IsTerminal(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}
