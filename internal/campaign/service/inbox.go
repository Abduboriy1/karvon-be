package service

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// threadFetchPages bounds how much of one conversation opening it pulls from
// Instantly: three pages of a hundred is far longer than any cold-email thread.
const threadFetchPages = 3

/* ------------------------------------------------------------------ inbox */

// InboxDirection names an Instantly email as received or sent, and reports false
// for a scheduled email, which has not happened yet and is not mirrored.
func InboxDirection(ueType int) (string, bool) {
	switch ueType {
	case instantly.EmailTypeReceived:
		return campaign.InboxReceived, true
	case instantly.EmailTypeSentFromCampaign, instantly.EmailTypeSent:
		return campaign.InboxSent, true
	default:
		return "", false
	}
}

// MirrorEmail upserts one Instantly email into the inbox, linked to our campaign
// and contact when they are ours. It reports whether the row is new; a scheduled
// email is skipped and reports false.
func (s *Service) MirrorEmail(ctx context.Context, e instantly.Email) (bool, error) {
	direction, ok := InboxDirection(e.UEType)
	if !ok || e.ID == "" {
		return false, nil
	}
	params := dbgen.UpsertInboxEmailParams{
		ID: e.ID, ThreadID: campaign.Optional(e.ThreadID), MessageID: campaign.Optional(e.MessageID),
		Direction: direction, UeType: campaign.Ptr(campaign.SignedInt32(e.UEType)),
		EmailAccount: campaign.Optional(strings.ToLower(strings.TrimSpace(e.EAccount))),
		LeadEmail:    campaign.Optional(strings.ToLower(strings.TrimSpace(e.LeadEmail))),
		FromAddress:  campaign.Optional(strings.ToLower(strings.TrimSpace(e.FromAddress))),
		ToAddresses:  campaign.Optional(e.ToAddressList), CcAddresses: campaign.Optional(e.CCAddressList),
		Subject: campaign.Optional(e.Subject), BodyText: campaign.Optional(e.Body.Text),
		BodyHtml: campaign.Optional(e.Body.HTML), ContentPreview: campaign.Optional(preview(e)),
		Step: campaign.Optional(e.Step), IsUnread: e.IsUnread != nil && *e.IsUnread == 1,
		IsAutoReply: e.IsAutoReply == 1, AiInterestValue: e.AIInterestValue,
		InstantlyCampaignID: campaign.Optional(e.CampaignID),
		SentAt:              e.TimestampEmail.UTC(), ProviderCreatedAt: e.TimestampCreated.UTC(),
	}
	if e.InterestStatus != nil {
		params.InterestStatus = campaign.Ptr(campaign.SignedInt32(*e.InterestStatus))
	}
	if params.SentAt.IsZero() {
		params.SentAt = params.ProviderCreatedAt
	}
	if params.ProviderCreatedAt.IsZero() {
		params.ProviderCreatedAt = params.SentAt
	}
	if e.CampaignID != "" {
		camp, err := s.store.GetCampaignByInstantlyID(ctx, &e.CampaignID)
		switch {
		case err == nil:
			params.CampaignID = uuid.NullUUID{UUID: camp.ID, Valid: true}
		case !errors.Is(err, pgx.ErrNoRows):
			return false, fmt.Errorf("inbox: load campaign: %w", err)
		}
	}
	if params.LeadEmail != nil {
		contact, err := s.store.GetContactByEmail(ctx, *params.LeadEmail)
		switch {
		case err == nil:
			params.ContactID = uuid.NullUUID{UUID: contact.ID, Valid: true}
		case !errors.Is(err, pgx.ErrNoRows):
			return false, fmt.Errorf("inbox: load contact: %w", err)
		}
	}
	inserted, err := s.store.UpsertInboxEmail(ctx, params)
	if err != nil {
		return false, fmt.Errorf("inbox: upsert email %s: %w", e.ID, err)
	}
	return inserted, nil
}

// preview is Instantly's own preview, or the first line or so of the text body
// when the list was fetched without one.
func preview(e instantly.Email) string {
	if p := strings.TrimSpace(e.ContentPreview); p != "" {
		return p
	}
	text := strings.Join(strings.Fields(e.Body.Text), " ")
	if r := []rune(text); len(r) > 200 {
		return string(r[:200])
	}
	return text
}

// InboxSince is where the next inbox sync starts: a little before the newest
// received email already stored, so an email Instantly committed out of order is
// not stepped over, or the backfill window when nothing is stored yet.
func (s *Service) InboxSince(ctx context.Context) (time.Time, error) {
	watermark, err := s.store.InboxWatermark(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("inbox: watermark: %w", err)
	}
	if watermark.Unix() <= 0 {
		return s.now().AddDate(0, 0, -s.cfg.InboxBackfillDays), nil
	}
	return watermark.Add(-10 * time.Minute), nil
}

// ListInbox pages the received emails.
func (s *Service) ListInbox(ctx context.Context, f db.InboxFilter, sort string, page, perPage int) (Page[db.InboxRow], error) {
	rows, err := s.store.ListInboxRows(ctx, f, sort, perPage, (page-1)*perPage)
	if err != nil {
		return Page[db.InboxRow]{}, apperr.Internal(err)
	}
	total, err := s.store.CountInboxRows(ctx, f)
	if err != nil {
		return Page[db.InboxRow]{}, apperr.Internal(err)
	}
	return Page[db.InboxRow]{Rows: rows, Total: total}, nil
}

// InboxUnreadCount counts the unread received emails.
func (s *Service) InboxUnreadCount(ctx context.Context) (int64, error) {
	n, err := s.store.CountInboxUnread(ctx)
	if err != nil {
		return 0, apperr.Internal(err)
	}
	return n, nil
}

// Thread is one conversation, oldest email first.
type Thread struct {
	ThreadID string
	Emails   []dbgen.InboxEmail
	// Stale is set when Instantly could not be asked for the full thread, so the
	// emails are only what was already mirrored.
	Stale bool
}

// GetThread loads a conversation. The first time a thread is opened its sent side
// is fetched from Instantly, which the inbox sync does not mirror; refresh asks
// again regardless. A failed fetch still returns what is stored.
func (s *Service) GetThread(ctx context.Context, threadID string, refresh bool) (Thread, error) {
	out := Thread{ThreadID: threadID}
	sent, err := s.store.CountInboxThreadSent(ctx, &threadID)
	if err != nil {
		return out, apperr.Internal(err)
	}
	if refresh || sent == 0 {
		if err := s.fetchThread(ctx, threadID); err != nil {
			s.log.Warn("could not fetch an inbox thread from Instantly", "thread_id", threadID, "error", err)
			out.Stale = true
		}
	}
	out.Emails, err = s.store.ListInboxThread(ctx, &threadID)
	if err != nil {
		return out, apperr.Internal(err)
	}
	if len(out.Emails) == 0 {
		return out, apperr.NotFound("thread")
	}
	return out, nil
}

func (s *Service) fetchThread(ctx context.Context, threadID string) error {
	client, err := s.Instantly(ctx)
	if err != nil {
		return err
	}
	cursor := ""
	for range threadFetchPages {
		if err := s.emails.Wait(ctx); err != nil {
			return err
		}
		page, err := client.ListEmails(ctx, instantly.ListEmailsInput{
			Search: instantly.ThreadSearch(threadID), Limit: 100, SortOrder: "asc", StartingAfter: cursor,
		})
		if err != nil {
			return fmt.Errorf("list thread: %w", err)
		}
		for _, e := range page.Items {
			// The thread search is a search: keep only what is really this thread.
			if e.ThreadID != "" && e.ThreadID != threadID {
				continue
			}
			if _, err := s.MirrorEmail(ctx, e); err != nil {
				return err
			}
		}
		if page.NextStartingAfter == "" || len(page.Items) == 0 {
			return nil
		}
		cursor = page.NextStartingAfter
	}
	return nil
}

// MarkThreadRead marks a conversation read at Instantly and here.
func (s *Service) MarkThreadRead(ctx context.Context, threadID string) error {
	emails, err := s.store.ListInboxThread(ctx, &threadID)
	if err != nil {
		return apperr.Internal(err)
	}
	if len(emails) == 0 {
		return apperr.NotFound("thread")
	}
	client, err := s.Instantly(ctx)
	if err != nil {
		return err
	}
	if err := client.MarkThreadRead(ctx, threadID); err != nil {
		return providerErr("Instantly could not mark the thread read", err)
	}
	if _, err := s.store.MarkInboxThreadRead(ctx, &threadID); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// SyncInbox queues an inbox sync now. Each call carries its own request id, so it
// runs even while the periodic pass is in flight.
func (s *Service) SyncInbox(ctx context.Context) error {
	return s.enqueue(ctx, campaign.SyncInboxArgs{RequestID: ids.New()})
}

/* --------------------------------------------------------------- outreach */

// OutreachPage is one page of the outcomes table, with the per-outcome totals of
// the same filter.
type OutreachPage struct {
	Page[db.OutreachRow]
	Outcomes map[string]int64
}

// ListOutreach pages the outcomes table.
func (s *Service) ListOutreach(ctx context.Context, f db.OutreachFilter, sort string, page, perPage int) (OutreachPage, error) {
	rows, err := s.store.ListOutreachRows(ctx, f, sort, perPage, (page-1)*perPage)
	if err != nil {
		return OutreachPage{}, apperr.Internal(err)
	}
	total, err := s.store.CountOutreachRows(ctx, f)
	if err != nil {
		return OutreachPage{}, apperr.Internal(err)
	}
	counts, err := s.store.OutreachOutcomeCounts(ctx, f)
	if err != nil {
		return OutreachPage{}, apperr.Internal(err)
	}
	for _, o := range campaign.Outcomes {
		if _, ok := counts[o]; !ok {
			counts[o] = 0
		}
	}
	return OutreachPage{Page: Page[db.OutreachRow]{Rows: rows, Total: total}, Outcomes: counts}, nil
}

// OutreachCSVHeader is the column order of the outcomes export.
var OutreachCSVHeader = []string{
	"email", "first_name", "last_name", "company", "title", "phone", "website", "campaign",
	"outcome", "interest_status", "lead_status", "sending_account", "emails_sent",
	"first_contacted_at", "last_contacted_at", "opens", "clicks", "replies", "last_replied_at",
	"last_reply_subject", "last_reply_preview", "lead_id", "contact_id", "campaign_id",
}

// ExportOutreach streams the outcomes table as CSV, flushing every 200 rows so a
// large export starts downloading at once. It returns the rows written.
func (s *Service) ExportOutreach(ctx context.Context, f db.OutreachFilter, sort string, dst io.Writer, flush func()) (int, error) {
	w := csv.NewWriter(dst)
	if err := w.Write(OutreachCSVHeader); err != nil {
		return 0, err
	}
	w.Flush()
	if flush != nil {
		flush()
	}
	written := 0
	err := s.store.StreamOutreachRows(ctx, f, sort, func(r db.OutreachRow) error {
		if err := w.Write(outreachCSVRecord(r)); err != nil {
			return err
		}
		written++
		if written%200 == 0 {
			w.Flush()
			if flush != nil {
				flush()
			}
		}
		return w.Error()
	})
	w.Flush()
	if flush != nil {
		flush()
	}
	if err != nil {
		return written, err
	}
	return written, w.Error()
}

// InterestLabelOf is the lead's stored interest label, or the name of its code
// when only the code was recorded.
func InterestLabelOf(label *string, code *int32) string {
	if label != nil && *label != "" {
		return *label
	}
	if code != nil {
		return instantly.InterestLabel(int(*code))
	}
	return ""
}

func outreachCSVRecord(r db.OutreachRow) []string {
	return []string{
		r.Email, deref(r.FirstName), deref(r.LastName), deref(r.Company), deref(r.Title), deref(r.Phone),
		deref(r.Website), r.CampaignName, r.Outcome, InterestLabelOf(r.InterestLabel, r.InterestStatus), r.Status,
		deref(r.SendingAccount), strconv.FormatInt(r.EmailsSent, 10), csvTime(r.FirstContactedAt),
		csvTime(r.LastContactedAt), strconv.Itoa(int(r.OpenCount)), strconv.Itoa(int(r.ClickCount)),
		strconv.Itoa(int(r.ReplyCount)), csvTime(r.LastRepliedAt), deref(r.LastReplySubject),
		deref(r.LastReplyPreview), r.LeadID.String(), r.ContactID.String(), r.CampaignID.String(),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func csvTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
