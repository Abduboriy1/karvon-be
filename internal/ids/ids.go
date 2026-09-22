// Package ids generates the UUIDv7 identifiers used for every resource. Version 7 is
// time-ordered, which keeps primary-key inserts append-friendly and makes ids sort
// chronologically in the API.
package ids

import "github.com/google/uuid"

// New returns a fresh UUIDv7. It falls back to v4 if the system clock source fails,
// which keeps id generation infallible for callers.
func New() uuid.UUID {
	if id, err := uuid.NewV7(); err == nil {
		return id
	}
	return uuid.New()
}
