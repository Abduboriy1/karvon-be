package db

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/db/dbgen"
)

// JobFilter is the filter set behind GET /jobs.
type JobFilter struct {
	Statuses []string
	SourceID *uuid.UUID
	From     *time.Time
	To       *time.Time
	Q        *string
}

// JobRow is a job joined with the provider it runs against.
type JobRow struct {
	ID         uuid.UUID
	Name       string
	Status     string
	SourceID   uuid.UUID
	SourceName string
	SourceKind string
	Config     []byte
	Stats      []byte
	Error      *string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

var jobSorts = map[string]sortSpec{
	"created_at:desc": {expr: "j.created_at", desc: true},
	"created_at:asc":  {expr: "j.created_at"},
	"name:asc":        {expr: "j.name"},
	"name:desc":       {expr: "j.name", desc: true},
	"status:asc":      {expr: "j.status"},
	"status:desc":     {expr: "j.status", desc: true},
	"started_at:desc": {expr: "j.started_at", desc: true},
	"started_at:asc":  {expr: "j.started_at"},
}

const jobSelectColumns = `
    j.id, j.name, j.status, j.source_id, s.name AS source_name, s.kind AS source_kind,
    j.config, j.stats, j.error, j.created_at, j.started_at, j.finished_at`

const jobFrom = " FROM jobs j JOIN sources s ON s.id = j.source_id"

func buildJobWhere(f JobFilter, a *argSet) string {
	conds := []string{"TRUE"}
	if len(f.Statuses) > 0 {
		conds = append(conds, "j.status = ANY("+a.add(f.Statuses)+")")
	}
	if f.SourceID != nil {
		conds = append(conds, "j.source_id = "+a.add(*f.SourceID))
	}
	if f.From != nil {
		conds = append(conds, "j.created_at >= "+a.add(*f.From))
	}
	if f.To != nil {
		conds = append(conds, "j.created_at <= "+a.add(*f.To))
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		conds = append(conds, "j.name ILIKE "+a.add("%"+strings.TrimSpace(*f.Q)+"%"))
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// CountJobs returns how many jobs match a filter.
func (s *Store) CountJobs(ctx context.Context, f JobFilter) (int64, error) {
	a := &argSet{}
	where := buildJobWhere(f, a)
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*)"+jobFrom+where, a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count jobs: %w", err)
	}
	return total, nil
}

// ListJobs returns one page of jobs.
func (s *Store) ListJobs(ctx context.Context, f JobFilter, sort string, limit, offset int) ([]JobRow, error) {
	a := &argSet{}
	where := buildJobWhere(f, a)
	spec := lookupSort(jobSorts, sort, "created_at:desc")

	q := "SELECT" + jobSelectColumns + jobFrom + where + spec.orderBy("j.id") +
		" LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)

	rows, err := s.pool.Query(ctx, q, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list jobs: %w", err)
	}
	defer rows.Close()

	out := make([]JobRow, 0, limit)
	for rows.Next() {
		var j JobRow
		if err := rows.Scan(&j.ID, &j.Name, &j.Status, &j.SourceID, &j.SourceName, &j.SourceKind,
			&j.Config, &j.Stats, &j.Error, &j.CreatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, fmt.Errorf("db: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetJobRow loads a single job with its provider, or pgx.ErrNoRows.
func (s *Store) GetJobRow(ctx context.Context, id uuid.UUID) (JobRow, error) {
	var j JobRow
	err := s.pool.QueryRow(ctx, "SELECT"+jobSelectColumns+jobFrom+" WHERE j.id = $1", id).
		Scan(&j.ID, &j.Name, &j.Status, &j.SourceID, &j.SourceName, &j.SourceKind,
			&j.Config, &j.Stats, &j.Error, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return JobRow{}, err
	}
	return j, nil
}

// JobSortKeys lists the accepted values of the /jobs `sort` parameter.
func JobSortKeys() []string { return sortKeys(jobSorts) }

// ClaimProviderRunSlot moves one job_query from "no run yet" to "starting", but only
// while the source has fewer than max runs in flight.
//
// The advisory lock is what makes the count trustworthy: without it two workers read
// the same free slot in their own snapshots and both start a run, and an extra run is
// an extra bill. The lock is held for the length of this transaction only, and it is
// keyed on the source, so two different vendors never wait on each other.
func (s *Store) ClaimProviderRunSlot(
	ctx context.Context,
	queryID, sourceID uuid.UUID,
	maxActive int,
) (bool, error) {
	if maxActive < 1 {
		maxActive = 1
	}

	var claimed bool
	err := s.InTx(ctx, func(q *dbgen.Queries) error {
		if err := q.LockProviderRunSlots(ctx, advisoryKey(sourceID)); err != nil {
			return fmt.Errorf("db: lock run slots: %w", err)
		}
		active, err := q.CountActiveProviderRuns(ctx, sourceID)
		if err != nil {
			return fmt.Errorf("db: count active runs: %w", err)
		}
		if active >= int64(maxActive) {
			return nil
		}
		rows, err := q.ClaimProviderRunSlot(ctx, queryID)
		if err != nil {
			return fmt.Errorf("db: claim run slot: %w", err)
		}
		claimed = rows > 0
		return nil
	})
	return claimed, err
}

// advisoryKey folds a UUID into the int64 an advisory lock takes. A collision between
// two sources would only mean they queue behind each other for a few milliseconds.
func advisoryKey(id uuid.UUID) int64 {
	b := id[:]
	return int64(binary.BigEndian.Uint64(b[:8]) ^ binary.BigEndian.Uint64(b[8:])) //nolint:gosec // G115: a wrapped hash is the point
}
