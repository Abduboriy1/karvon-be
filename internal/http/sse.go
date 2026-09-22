package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/http/gen"
)

const (
	// sseBatchSize is how many stored events are read per database round trip.
	sseBatchSize = 500
	// sseBacklog is how much history a client that does not ask for a specific
	// position receives on connect.
	sseBacklog = 200
	// ssePollInterval is a safety net in case a NOTIFY is missed (for example while
	// the listener was reconnecting).
	ssePollInterval = 3 * time.Second
)

// StreamJobEvents implements GET /jobs/{id}/events.
//
// The stream replays from `Last-Event-ID` (or `?after=`), then follows the job live
// via LISTEN/NOTIFY, sends a comment ping every 15 seconds to keep proxies from
// closing the connection, and ends once the job reaches a terminal status.
func (s *Server) StreamJobEvents(w http.ResponseWriter, r *http.Request, id gen.IdPath, params gen.StreamJobEventsParams) {
	ctx := r.Context()

	job, err := s.jobs.Get(ctx, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, r, apperr.Internal(fmt.Errorf("sse: response writer does not support flushing")))
		return
	}

	lastID, explicit := resolveLastEventID(params)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Tell nginx not to buffer the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var (
		notify <-chan struct{}
		cancel = func() {}
	)
	if s.listener != nil {
		notify, cancel = s.listener.Subscribe(id)
	}
	defer cancel()

	// A job that is already finished gets its history and then a clean close.
	terminal := events.IsTerminal(job.Status)

	if !explicit {
		backlogID, sawTerminal, err := s.writeBacklog(ctx, w, flusher, id)
		if err != nil {
			s.log.Warn("sse backlog failed", "job_id", id, "error", err)
			return
		}
		lastID = backlogID
		if sawTerminal {
			return
		}
	}

	ping := time.NewTicker(s.cfg.SSEPingInterval)
	defer ping.Stop()
	poll := time.NewTicker(ssePollInterval)
	defer poll.Stop()

	for {
		nextID, sawTerminal, err := s.drain(ctx, w, flusher, id, lastID)
		if err != nil {
			s.log.Warn("sse drain failed", "job_id", id, "error", err)
			return
		}
		lastID = nextID
		if sawTerminal || terminal {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-poll.C:
			// Cheap status check so a stream for an already-finished job closes even
			// if its terminal event predates this connection.
			if status, err := s.store.GetJobStatus(ctx, id); err == nil && events.IsTerminal(status) {
				terminal = true
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// resolveLastEventID reads the resume position from the header or the query parameter.
func resolveLastEventID(params gen.StreamJobEventsParams) (int64, bool) {
	if params.LastEventID != nil && *params.LastEventID != "" {
		if id, err := strconv.ParseInt(*params.LastEventID, 10, 64); err == nil && id >= 0 {
			return id, true
		}
	}
	if params.After != nil && *params.After >= 0 {
		return *params.After, true
	}
	return 0, false
}

// writeBacklog sends the tail of the job's history to a client that connected without
// a resume position. It returns the id it stopped at and whether that history already
// contained the job's terminal status, in which case there is nothing left to stream.
func (s *Server) writeBacklog(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, jobID uuid.UUID) (int64, bool, error) {
	rows, err := s.store.ListRecentJobEvents(ctx, dbgen.ListRecentJobEventsParams{
		JobID: jobID,
		Lim:   sseBacklog,
	})
	if err != nil {
		return 0, false, err
	}

	var (
		lastID      int64
		sawTerminal bool
	)
	// The query returns newest first; replay in chronological order.
	for i := len(rows) - 1; i >= 0; i-- {
		if err := writeSSEEvent(w, rows[i].ID, rows[i].Type, rows[i].Data); err != nil {
			return lastID, false, err
		}
		lastID = rows[i].ID
		if isTerminalEvent(rows[i].Type, rows[i].Data) {
			sawTerminal = true
			break
		}
	}
	flusher.Flush()
	return lastID, sawTerminal, nil
}

// drain writes every stored event newer than afterID and reports whether one of them
// ended the job.
func (s *Server) drain(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, jobID uuid.UUID, afterID int64) (int64, bool, error) {
	for {
		rows, err := s.store.ListJobEventsAfter(ctx, dbgen.ListJobEventsAfterParams{
			JobID:   jobID,
			AfterID: afterID,
			Lim:     sseBatchSize,
		})
		if err != nil {
			return afterID, false, err
		}
		if len(rows) == 0 {
			return afterID, false, nil
		}

		for _, row := range rows {
			if err := writeSSEEvent(w, row.ID, row.Type, row.Data); err != nil {
				return afterID, false, err
			}
			afterID = row.ID
			if isTerminalEvent(row.Type, row.Data) {
				flusher.Flush()
				return afterID, true, nil
			}
		}
		flusher.Flush()

		if len(rows) < sseBatchSize {
			return afterID, false, nil
		}
		if err := ctx.Err(); err != nil {
			return afterID, false, err
		}
	}
}

// writeSSEEvent renders one event in the text/event-stream wire format.
func writeSSEEvent(w http.ResponseWriter, id int64, eventType string, data []byte) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	_, err := fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", eventType, id, data)
	return err
}

func isTerminalEvent(eventType string, data []byte) bool {
	if eventType != string(events.TypeStatus) {
		return false
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	return events.IsTerminal(payload.Status)
}
