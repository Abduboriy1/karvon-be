package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Listener holds one dedicated connection LISTENing on the job_events channel and
// wakes every in-process SSE subscriber for the job that produced an event.
type Listener struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu     sync.Mutex
	nextID int64
	subs   map[uuid.UUID]map[int64]chan struct{}
}

// NewListener builds a Listener. Call Run to start it.
func NewListener(pool *pgxpool.Pool, log *slog.Logger) *Listener {
	return &Listener{
		pool: pool,
		log:  log,
		subs: make(map[uuid.UUID]map[int64]chan struct{}),
	}
}

// Subscribe registers interest in one job. The returned channel receives an empty
// struct whenever new events may be available; it is never closed by the listener.
// Call the returned function to unsubscribe.
func (l *Listener) Subscribe(jobID uuid.UUID) (<-chan struct{}, func()) {
	// Buffered by one: a pending wake-up is enough, extra notifications collapse.
	ch := make(chan struct{}, 1)

	l.mu.Lock()
	l.nextID++
	id := l.nextID
	if l.subs[jobID] == nil {
		l.subs[jobID] = make(map[int64]chan struct{})
	}
	l.subs[jobID][id] = ch
	l.mu.Unlock()

	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if m, ok := l.subs[jobID]; ok {
			delete(m, id)
			if len(m) == 0 {
				delete(l.subs, jobID)
			}
		}
	}
}

func (l *Listener) wake(jobID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ch := range l.subs[jobID] {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
}

// Run blocks until ctx is cancelled, reconnecting with backoff on connection loss.
func (l *Listener) Run(ctx context.Context) error {
	backoff := 250 * time.Millisecond
	const maxBackoff = 10 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := l.listenOnce(ctx)
		switch {
		case err == nil, errors.Is(err, context.Canceled):
			return nil
		default:
			l.log.Warn("job event listener disconnected, retrying", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

func (l *Listener) listenOnce(ctx context.Context) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection is dedicated to LISTEN; destroy it rather than returning a
	// connection with session state back to the pool.
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	l.log.Info("listening for job events", "channel", Channel)

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		jobID, err := uuid.Parse(notification.Payload)
		if err != nil {
			l.log.Warn("ignoring job event notification with bad payload", "payload", notification.Payload)
			continue
		}
		l.wake(jobID)
	}
}
