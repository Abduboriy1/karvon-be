package integration_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
)

// checkWith asserts that every Go constant is accepted by the column's CHECK
// constraint and that an invented value is not. reset, when given, runs before
// each value, for columns whose row is unique per contact.
func checkWith(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name, query string,
	values []string, invalid string, reset func() error, args ...any,
) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		for _, v := range values {
			if reset != nil {
				if err := reset(); err != nil {
					t.Fatalf("could not reset before %q: %v", v, err)
				}
			}
			if _, err := pool.Exec(ctx, query, append([]any{v}, args...)...); err != nil {
				t.Errorf("the database rejected %s %q: %v", name, v, err)
			}
		}
		if reset != nil {
			if err := reset(); err != nil {
				t.Fatalf("could not reset before the invalid value: %v", err)
			}
		}
		if _, err := pool.Exec(ctx, query, append([]any{invalid}, args...)...); err == nil {
			t.Errorf("the database accepted the unknown %s %q", name, invalid)
		}
		if reset != nil {
			if err := reset(); err != nil {
				t.Fatalf("could not reset after %s: %v", name, err)
			}
		}
	})
}

// The campaign schema encodes its enums as CHECK constraints and the Go code
// encodes them as constants. These tests keep the two in step: every Go value must
// be accepted and an invented one must be refused, so renaming a constant without
// a migration fails here rather than in production.
func TestCampaignSchemaMatchesTheGoConstants(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()
	pool := h.app.Store().Pool()

	// One contact and one campaign to hang the rest off.
	var contactID, campaignID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO contacts (id, email, domain) VALUES (gen_random_uuid(), 'schema@example.test', 'example.test')
		RETURNING id`).Scan(&contactID); err != nil {
		t.Fatalf("could not seed a contact: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaigns (id, name) VALUES (gen_random_uuid(), 'Schema check') RETURNING id`).Scan(&campaignID); err != nil {
		t.Fatalf("could not seed a campaign: %v", err)
	}
	var leadID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO campaign_leads (id, campaign_id, contact_id) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		campaignID, contactID).Scan(&leadID); err != nil {
		t.Fatalf("could not seed a campaign lead: %v", err)
	}

	check := func(t *testing.T, name, query string, values []string, invalid string, args ...any) {
		t.Helper()
		checkWith(ctx, t, pool, name, query, values, invalid, nil, args...)
	}

	// Lifecycle stages. The trigger blocks the newsletter stages without consent,
	// pins a suppressed contact to a terminal stage and refuses to undo an
	// unsubscribe, so each of those is checked on its own contact below.
	t.Run("every lifecycle stage is accepted", func(t *testing.T) {
		for _, stage := range campaign.Stages {
			if stage.Newsletter() {
				continue // needs a consent row; covered by the trigger tests
			}
			// One contact per stage, because several of them are one-way.
			var id string
			if err := pool.QueryRow(ctx, `
				INSERT INTO contacts (id, email, domain) VALUES (gen_random_uuid(), $1, 'example.test') RETURNING id`,
				"stage-"+string(stage)+"@example.test").Scan(&id); err != nil {
				t.Fatalf("could not seed a contact for %q: %v", stage, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE contacts SET lifecycle_stage = $1 WHERE id = $2`, string(stage), id); err != nil {
				t.Errorf("the database rejected the stage %q: %v", stage, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE contacts SET lifecycle_stage = 'daydreaming' WHERE id = $1`, contactID); err == nil {
			t.Error("an unknown lifecycle stage was accepted")
		}
	})

	check(t, "campaign status", `UPDATE campaigns SET status = $1 WHERE id = $2`, campaign.CampaignStatuses, "sulking", campaignID)
	check(t, "campaign lead status", `UPDATE campaign_leads SET status = $1 WHERE id = $2`, campaign.LeadStatuses, "wandering", leadID)
	check(t, "contact source", `UPDATE contacts SET source = $1 WHERE id = $2`, campaign.ContactSources, "telepathy", contactID)

	// A contact holds at most one active consent and one active suppression, so
	// each value is inserted and cleared in turn.
	clearConsents := func() error {
		_, err := pool.Exec(ctx, `DELETE FROM contact_consents WHERE contact_id = $1`, contactID)
		return err
	}
	checkWith(ctx, t, pool, "consent source", `
		INSERT INTO contact_consents (id, contact_id, source, captured_at, evidence, captured_by)
		VALUES (gen_random_uuid(), $2, $1, now(), 'evidence', 'tester')`,
		campaign.ConsentSources, "vibes", clearConsents, contactID)

	clearSuppressions := func() error {
		_, err := pool.Exec(ctx, `DELETE FROM contact_suppressions WHERE contact_id = $1`, contactID)
		return err
	}
	checkWith(ctx, t, pool, "suppression reason", `
		INSERT INTO contact_suppressions (id, contact_id, reason, source)
		VALUES (gen_random_uuid(), $2, $1, 'manual')`, campaign.SuppressionReasons, "boredom", clearSuppressions, contactID)
	checkWith(ctx, t, pool, "suppression source", `
		INSERT INTO contact_suppressions (id, contact_id, reason, source)
		VALUES (gen_random_uuid(), $2, 'do_not_contact', $1)`, campaign.SuppressionSources, "hearsay", clearSuppressions, contactID)

	check(t, "component type", `
		INSERT INTO email_components (id, type, name, body) VALUES (gen_random_uuid(), $1, 'n', 'b')`,
		campaign.ComponentTypes, "haiku")
	check(t, "content status", `
		INSERT INTO email_components (id, type, name, body, status) VALUES (gen_random_uuid(), 'subject', 'n', 'b', $1)`,
		campaign.ContentStatuses, "perfected")
	check(t, "variant status", `
		INSERT INTO email_variants (id, name, status, subject_template, body_template)
		VALUES (gen_random_uuid(), 'v', $1, 's', 'b')`, campaign.ContentStatuses, "perfected")

	check(t, "contact event type", `
		INSERT INTO contact_events (contact_id, type, occurred_at, source) VALUES ($2, $1, now(), 'system')`,
		campaign.EventTypes, "telepathy", contactID)
	check(t, "contact event source", `
		INSERT INTO contact_events (contact_id, type, occurred_at, source) VALUES ($2, 'note', now(), $1)`,
		campaign.EventSources, "rumour", contactID)

	check(t, "provider name", `
		INSERT INTO provider_events (id, provider, event_type, dedupe_key, raw, source)
		VALUES (gen_random_uuid(), $1, 'email_sent', gen_random_uuid()::text, '{}'::jsonb, 'webhook')`,
		[]string{campaign.ProviderInstantly, campaign.ProviderMailchimp}, "carrier_pigeon")
	check(t, "provider event source", `
		INSERT INTO provider_events (id, provider, event_type, dedupe_key, raw, source)
		VALUES (gen_random_uuid(), 'instantly', 'email_sent', gen_random_uuid()::text, '{}'::jsonb, $1)`,
		[]string{campaign.ProviderEventWebhook, campaign.ProviderEventReplay, campaign.ProviderEventTest}, "seance")

	check(t, "sync run kind", `
		INSERT INTO sync_runs (id, kind) VALUES (gen_random_uuid(), $1)`, campaign.SyncKinds, "astrology")
	check(t, "generation provider", `
		INSERT INTO ai_generations (id, provider, prompt, prompt_version) VALUES (gen_random_uuid(), $1, 'p', 'v')`,
		[]string{campaign.AIProviderManual, campaign.AIProviderOpenAI, campaign.AIProviderChatGPT}, "oracle")
	check(t, "generation status", `
		INSERT INTO ai_generations (id, provider, status, prompt, prompt_version)
		VALUES (gen_random_uuid(), 'manual_chatgpt', $1, 'p', 'v')`, campaign.GenerationStatuses, "pondered")

	// Sends and subscriptions need their own parents.
	t.Run("reply classifications and send sources are accepted", func(t *testing.T) {
		var sendID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO email_sends (id, campaign_lead_id, campaign_id, contact_id, step, sent_at, source)
			VALUES (gen_random_uuid(), $1, $2, $3, 1, now(), 'webhook') RETURNING id`,
			leadID, campaignID, contactID).Scan(&sendID); err != nil {
			t.Fatalf("could not seed a send: %v", err)
		}
		for _, c := range campaign.ReplyClassifications {
			if _, err := pool.Exec(ctx, `UPDATE email_sends SET reply_classification = $1 WHERE id = $2`, c, sendID); err != nil {
				t.Errorf("the database rejected the classification %q: %v", c, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE email_sends SET reply_classification = 'enthusiastic' WHERE id = $1`, sendID); err == nil {
			t.Error("an unknown reply classification was accepted")
		}
		for _, src := range []string{campaign.SendSourceWebhook, campaign.SendSourceReconcile} {
			if _, err := pool.Exec(ctx, `UPDATE email_sends SET source = $1 WHERE id = $2`, src, sendID); err != nil {
				t.Errorf("the database rejected the send source %q: %v", src, err)
			}
		}
	})

	t.Run("subscription statuses are accepted", func(t *testing.T) {
		var audienceID, subID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO newsletter_audiences (id, mailchimp_list_id, name) VALUES (gen_random_uuid(), 'list-schema', 'Schema')
			RETURNING id`).Scan(&audienceID); err != nil {
			t.Fatalf("could not seed an audience: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO newsletter_subscriptions (id, contact_id, audience_id, requested_status)
			VALUES (gen_random_uuid(), $1, $2, 'pending') RETURNING id`, contactID, audienceID).Scan(&subID); err != nil {
			t.Fatalf("could not seed a subscription: %v", err)
		}
		for _, st := range campaign.SubscriptionStatuses {
			if _, err := pool.Exec(ctx, `UPDATE newsletter_subscriptions SET status = $1 WHERE id = $2`, st, subID); err != nil {
				t.Errorf("the database rejected the subscription status %q: %v", st, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE newsletter_subscriptions SET status = 'ambivalent' WHERE id = $1`, subID); err == nil {
			t.Error("an unknown subscription status was accepted")
		}
		for _, st := range campaign.SyncStatuses {
			if _, err := pool.Exec(ctx, `UPDATE newsletter_subscriptions SET sync_status = $1 WHERE id = $2`, st, subID); err != nil {
				t.Errorf("the database rejected the sync status %q: %v", st, err)
			}
		}
		for _, st := range []string{campaign.RequestPending, campaign.RequestUnsubscribed} {
			if _, err := pool.Exec(ctx, `UPDATE newsletter_subscriptions SET requested_status = $1 WHERE id = $2`, st, subID); err != nil {
				t.Errorf("the database rejected the requested status %q: %v", st, err)
			}
		}
		// subscribed without a consent row is refused by a CHECK constraint.
		if _, err := pool.Exec(ctx, `UPDATE newsletter_subscriptions SET requested_status = 'subscribed' WHERE id = $1`, subID); err == nil {
			t.Error("a subscribed request without a consent id was accepted")
		}
	})

	t.Run("the source table accepts the campaign providers", func(t *testing.T) {
		var kind, role string
		if err := pool.QueryRow(ctx, `SELECT kind, role FROM sources WHERE id = $1`, instantlySourceID).Scan(&kind, &role); err != nil {
			t.Fatalf("the Instantly source row is missing: %v", err)
		}
		if kind != campaign.KindInstantly || role != campaign.RoleOutreach {
			t.Errorf("the Instantly source is %s/%s, want %s/%s", kind, role, campaign.KindInstantly, campaign.RoleOutreach)
		}
		if err := pool.QueryRow(ctx, `SELECT kind, role FROM sources WHERE id = $1`, mailchimpSourceID).Scan(&kind, &role); err != nil {
			t.Fatalf("the Mailchimp source row is missing: %v", err)
		}
		if kind != campaign.KindMailchimp || role != campaign.RoleNewsletter {
			t.Errorf("the Mailchimp source is %s/%s, want %s/%s", kind, role, campaign.KindMailchimp, campaign.RoleNewsletter)
		}
	})

	t.Run("the AI modes are the two the schema knows", func(t *testing.T) {
		if ai.ModeManual != "manual" || ai.ModeAPI != "api" {
			t.Fatalf("the AI modes changed: %q / %q", ai.ModeManual, ai.ModeAPI)
		}
	})
}

// The guards are the last line of defence behind the service layer, so they are
// tested against the database directly.
func TestTheDatabaseRefusesToUndoAnUnsubscribe(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()
	pool := h.app.Store().Pool()

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO contacts (id, email, domain, lifecycle_stage, suppressed_at, suppression_reason)
		VALUES (gen_random_uuid(), 'gone@example.test', 'example.test', 'unsubscribed', now(), 'unsubscribed')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("could not seed an unsubscribed contact: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE contacts SET lifecycle_stage = 'contacted' WHERE id = $1`, id); err == nil {
		t.Error("an unsubscribed contact was moved back into the funnel")
	}
	if _, err := pool.Exec(ctx, `UPDATE contacts SET suppressed_at = NULL, suppression_reason = NULL WHERE id = $1`, id); err == nil {
		t.Error("the unsubscribe suppression was lifted")
	}
}

func TestTheDatabaseRefusesANewsletterStageWithoutConsent(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()
	pool := h.app.Store().Pool()

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO contacts (id, email, domain, lifecycle_stage)
		VALUES (gen_random_uuid(), 'keen@example.test', 'example.test', 'interested') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("could not seed a contact: %v", err)
	}

	// Interest is not consent: without a consent row the newsletter stages are shut.
	for _, stage := range []campaign.Stage{campaign.StageNewsletterEligible, campaign.StageMailchimpPending, campaign.StageMailchimpSubscribed} {
		if _, err := pool.Exec(ctx, `UPDATE contacts SET lifecycle_stage = $1 WHERE id = $2`, string(stage), id); err == nil {
			t.Errorf("an interested contact reached %s with no consent record", stage)
		}
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO contact_consents (id, contact_id, source, captured_at, evidence, captured_by)
		VALUES (gen_random_uuid(), $1, 'explicit_reply', now(), 'yes please send me the newsletter', 'tester')`, id); err != nil {
		t.Fatalf("could not record consent: %v", err)
	}
	for _, stage := range []campaign.Stage{campaign.StageNewsletterEligible, campaign.StageMailchimpPending, campaign.StageMailchimpSubscribed} {
		if _, err := pool.Exec(ctx, `UPDATE contacts SET lifecycle_stage = $1 WHERE id = $2`, string(stage), id); err != nil {
			t.Errorf("with consent recorded, %s was still refused: %v", stage, err)
		}
	}
}

func TestTheDatabaseRefusesToRewriteALockedAssignment(t *testing.T) {
	h, _ := seededHarness(t)
	ctx := context.Background()
	pool := h.app.Store().Pool()

	var contactID, campaignID, leadID, variantA, variantB, assignmentID string
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("could not seed %s: %v", what, err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO contacts (id, email, domain) VALUES (gen_random_uuid(), 'locked@example.test', 'example.test') RETURNING id`).Scan(&contactID), "a contact")
	must(pool.QueryRow(ctx, `INSERT INTO campaigns (id, name) VALUES (gen_random_uuid(), 'Locked') RETURNING id`).Scan(&campaignID), "a campaign")
	must(pool.QueryRow(ctx, `INSERT INTO campaign_leads (id, campaign_id, contact_id) VALUES (gen_random_uuid(), $1, $2) RETURNING id`, campaignID, contactID).Scan(&leadID), "a lead")
	must(pool.QueryRow(ctx, `INSERT INTO email_variants (id, name, subject_template, body_template) VALUES (gen_random_uuid(), 'A', 'Subject A', 'Body A') RETURNING id`).Scan(&variantA), "variant A")
	must(pool.QueryRow(ctx, `INSERT INTO email_variants (id, name, subject_template, body_template) VALUES (gen_random_uuid(), 'B', 'Subject B', 'Body B') RETURNING id`).Scan(&variantB), "variant B")
	must(pool.QueryRow(ctx, `
		INSERT INTO variant_assignments (id, campaign_lead_id, step, variant_id, weights_version, seed_hash, rendered_subject, rendered_body, locked_at)
		VALUES (gen_random_uuid(), $1, 1, $2, 1, 'seed', 'Subject A', 'Body A', now()) RETURNING id`,
		leadID, variantA).Scan(&assignmentID), "a locked assignment")

	if _, err := pool.Exec(ctx, `UPDATE variant_assignments SET variant_id = $1 WHERE id = $2`, variantB, assignmentID); err == nil {
		t.Error("a locked assignment was pointed at a different variant")
	}
	if _, err := pool.Exec(ctx, `UPDATE variant_assignments SET rendered_subject = 'Rewritten' WHERE id = $1`, assignmentID); err == nil {
		t.Error("the rendered subject of a locked assignment was rewritten")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM variant_assignments WHERE id = $1`, assignmentID); err == nil {
		t.Error("a locked assignment was deleted")
	}
}
