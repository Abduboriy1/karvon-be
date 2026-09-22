package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/verify"
)

// explain returns the query plan for a statement as a single string.
func explain(t *testing.T, h *harness, sql string, args ...any) string {
	t.Helper()

	ctx := context.Background()
	rows, err := h.app.Store().Pool().Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

// seedBusinesses inserts a realistic number of rows so the planner's choices reflect
// production rather than a three-row table.
func seedBusinesses(t *testing.T, h *harness, n int) {
	t.Helper()

	ctx := context.Background()
	pool := h.app.Store().Pool()

	if _, err := pool.Exec(ctx, `
		INSERT INTO businesses (id, place_id, name, category, city, state, phone, website, domain)
		SELECT gen_random_uuid(),
		       'seed-' || g,
		       'Fitness Studio ' || g,
		       'Gym',
		       'City ' || (g % 100),
		       'TX',
		       '+1512555' || lpad((g % 10000)::text, 4, '0'),
		       'https://studio' || g || '.example.test',
		       'studio' || g || '.example.test'
		FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatalf("could not seed businesses: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE businesses`); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessSearchUsesTheTrigramIndexAtScale(t *testing.T) {
	h, _ := seededHarness(t)
	seedBusinesses(t, h, 50_000)

	ctx := context.Background()
	total, err := h.app.Store().CountBusinesses(ctx, db.BusinessFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total < 50_000 {
		t.Fatalf("only %d rows were seeded", total)
	}

	// No planner hints: this is the plan production would get.
	plan := explain(t, h,
		`SELECT b.id FROM businesses b
		 WHERE (b.name ILIKE $1 OR b.domain ILIKE $1) AND b.suppressed = false
		 ORDER BY b.created_at DESC, b.id DESC LIMIT 50`,
		"%iron%")

	if strings.Contains(plan, "Seq Scan on businesses") {
		t.Fatalf("the ?q= search falls back to a sequential scan at 50k rows:\n%s", plan)
	}
	if !strings.Contains(plan, "trgm_idx") {
		t.Fatalf("no trigram index was chosen:\n%s", plan)
	}

	// And the endpoint itself still answers quickly.
	start := time.Now()
	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?q=iron&per_page=50", "", http.StatusOK)
	elapsed := time.Since(start)

	payload := decodeBody[businessListPayload](t, rec)
	if payload.Meta.Total == 0 {
		t.Fatal("the search returned nothing; the seeded rows should not have displaced the real ones")
	}
	t.Logf("GET /businesses?q=iron over %d rows took %s", total, elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("the search took %s over %d rows", elapsed, total)
	}
}

func TestJobListingOrdersByTheCreatedAtIndex(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()

	if _, err := h.app.Store().Pool().Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}

	plan := explain(t, h, `SELECT id FROM jobs ORDER BY created_at DESC LIMIT 50`)
	if !strings.Contains(plan, "jobs_created_at_idx") {
		t.Fatalf("the default job ordering does not use its index:\n%s", plan)
	}
}

func TestSchemaConstraints(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()
	pool := h.app.Store().Pool()

	t.Run("place_id is unique", func(t *testing.T) {
		var existing string
		if err := pool.QueryRow(ctx,
			`SELECT place_id FROM businesses WHERE place_id IS NOT NULL LIMIT 1`).Scan(&existing); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO businesses (id, place_id, name) VALUES (gen_random_uuid(), $1, 'Duplicate')`, existing)
		if err == nil {
			t.Fatal("a second row with the same place_id was accepted")
		}
	})

	t.Run("at most one primary email per business", func(t *testing.T) {
		var businessID string
		if err := pool.QueryRow(ctx,
			`SELECT business_id FROM business_emails WHERE is_primary LIMIT 1`).Scan(&businessID); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO business_emails (id, business_id, email, source, is_primary)
			 VALUES (gen_random_uuid(), $1, 'second@example.test', 'regex', true)`, businessID)
		if err == nil {
			t.Fatal("a second primary address was accepted for one business")
		}
	})

	t.Run("emails are case insensitive", func(t *testing.T) {
		var businessID string
		if err := pool.QueryRow(ctx, `SELECT business_id FROM business_emails LIMIT 1`).Scan(&businessID); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT email::text FROM business_emails WHERE business_id = $1 LIMIT 1`, businessID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO business_emails (id, business_id, email, source)
			 VALUES (gen_random_uuid(), $1, $2, 'regex')`, businessID, strings.ToUpper(stored))
		if err == nil {
			t.Fatalf("%q was accepted alongside %q: the citext unique constraint is not working",
				strings.ToUpper(stored), stored)
		}
	})

	t.Run("a source in use cannot be deleted", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM sources WHERE id = $1`, apifySourceID)
		if err == nil {
			t.Fatal("deleting a source that jobs reference should be restricted")
		}
	})

	t.Run("job status is constrained", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO jobs (id, name, status, source_id, config)
			 VALUES (gen_random_uuid(), 'Bad status', 'sideways', $1, '{}'::jsonb)`, apifySourceID)
		if err == nil {
			t.Fatal("an unknown job status was accepted")
		}
	})
}

func TestBusinessesSurviveWhenTheirFirstJobIsDeleted(t *testing.T) {
	h, jobID := seededHarness(t)

	h.mustRequest(http.MethodDelete, "/api/v1/jobs/"+jobID, "", http.StatusNoContent)

	total, err := h.app.Store().CountBusinesses(context.Background(), db.BusinessFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("businesses = %d, want 3 after the job was deleted", total)
	}

	// first_job_id is nulled rather than cascading the business away.
	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses", "", http.StatusOK)
	payload := decodeBody[struct {
		Data []struct {
			FirstJobID   *string `json:"first_job_id"`
			FirstJobName *string `json:"first_job_name"`
		} `json:"data"`
	}](t, rec)
	for _, row := range payload.Data {
		if row.FirstJobID != nil {
			t.Errorf("first_job_id = %v, want null after the job was deleted", *row.FirstJobID)
		}
		if row.FirstJobName != nil {
			t.Errorf("first_job_name = %v, want null", *row.FirstJobName)
		}
	}
}

// The tag and status vocabularies live in three places: the Go constants, the
// database CHECK constraints and the OpenAPI enums. This pins the first two
// together, so a new tag cannot be added in Go and silently rejected by Postgres.
func TestVerificationSchemaMatchesTheGoConstants(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()
	pool := h.app.Store().Pool()

	seed := func(t *testing.T, email string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO email_verifications (id, email, domain)
			VALUES (gen_random_uuid(), $1, 'example.test')
			RETURNING id`, email).Scan(&id); err != nil {
			t.Fatalf("could not seed %s: %v", email, err)
		}
		return id
	}

	t.Run("every Go tag is accepted", func(t *testing.T) {
		id := seed(t, "tags@example.test")
		for _, tag := range verify.Tags {
			if _, err := pool.Exec(ctx,
				`UPDATE email_verifications SET verification_tag = $1 WHERE id = $2`, string(tag), id); err != nil {
				t.Errorf("the database rejected the tag %q: %v", tag, err)
			}
		}
		if _, err := pool.Exec(ctx,
			`UPDATE email_verifications SET verification_tag = 'chartreuse' WHERE id = $1`, id); err == nil {
			t.Error("an unknown tag was accepted")
		}
	})

	t.Run("every conclusive pass 2 score is accepted", func(t *testing.T) {
		id := seed(t, "scores@example.test")
		for _, status := range []verify.Pass2Status{
			verify.Pass2Deliverable, verify.Pass2Risky, verify.Pass2Undeliverable,
		} {
			score, ok := verify.ScoreFor(status)
			if !ok {
				t.Fatalf("%q should be conclusive", status)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE email_verifications SET pass2_score = $1, pass2_status = $2 WHERE id = $3`,
				score, string(status), id); err != nil {
				t.Errorf("the database rejected %q / %d: %v", status, score, err)
			}
		}
		for _, status := range []verify.Pass2Status{verify.Pass2Unknown, verify.Pass2Error} {
			if _, err := pool.Exec(ctx,
				`UPDATE email_verifications SET pass2_status = $1 WHERE id = $2`,
				string(status), id); err != nil {
				t.Errorf("the database rejected the status %q: %v", status, err)
			}
		}
		// A score off our scale is a bug, and the column says so.
		if _, err := pool.Exec(ctx,
			`UPDATE email_verifications SET pass2_score = 55 WHERE id = $1`, id); err == nil {
			t.Error("a score outside the mapped scale was accepted")
		}
	})

	t.Run("the local score cannot exceed its maximum", func(t *testing.T) {
		id := seed(t, "ceiling@example.test")
		if _, err := pool.Exec(ctx,
			`UPDATE email_verifications SET pass1_score = $1 WHERE id = $2`,
			verify.Pass1MaxScore, id); err != nil {
			t.Errorf("the database rejected a perfect local score: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE email_verifications SET pass1_score = $1 WHERE id = $2`,
			verify.Pass1MaxScore+1, id); err == nil {
			t.Errorf("a local score above %d was accepted", verify.Pass1MaxScore)
		}
	})

	t.Run("the free score cannot reach the Verified band", func(t *testing.T) {
		// The column is the last line of defence behind the scoring layer's cap: a
		// weighted free score reaching 90 would tag an address "Verified" without
		// anyone having paid a provider to stand behind it.
		id := seed(t, "freeceiling@example.test")
		if _, err := pool.Exec(ctx,
			`UPDATE email_verifications SET free_score = $1 WHERE id = $2`,
			verify.FreeMaxScore, id); err != nil {
			t.Errorf("the database rejected a perfect free score: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE email_verifications SET free_score = $1 WHERE id = $2`,
			verify.FreeMaxScore+1, id); err == nil {
			t.Errorf("a free score above %d was accepted", verify.FreeMaxScore)
		}
		if verify.TagFor(verify.FreeMaxScore) == verify.TagGreen {
			t.Errorf("a free score of %d tags green", verify.FreeMaxScore)
		}
	})

	t.Run("the settings row exists and holds one row only", func(t *testing.T) {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM verification_settings`).Scan(&count); err != nil {
			t.Fatalf("could not read the settings table: %v", err)
		}
		if count != 1 {
			t.Errorf("verification_settings holds %d rows, want exactly 1", count)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO verification_settings (id) VALUES (2)`); err == nil {
			t.Error("a second settings row was accepted")
		}
	})

	t.Run("an address is unique and case insensitive", func(t *testing.T) {
		seed(t, "unique@example.test")
		if _, err := pool.Exec(ctx, `
			INSERT INTO email_verifications (id, email, domain)
			VALUES (gen_random_uuid(), 'UNIQUE@EXAMPLE.TEST', 'example.test')`); err == nil {
			t.Error("the same address was stored twice in different cases")
		}
	})

	t.Run("the verifier source is seeded and is not a maps provider", func(t *testing.T) {
		var role, kind string
		if err := pool.QueryRow(ctx,
			`SELECT role, kind FROM sources WHERE id = $1`, verifierSourceID).Scan(&role, &kind); err != nil {
			t.Fatalf("the verifier source was not seeded: %v", err)
		}
		if role != verify.RoleVerifier || kind != verify.KindEmailable {
			t.Errorf("verifier source = %q / %q", role, kind)
		}
	})
}
