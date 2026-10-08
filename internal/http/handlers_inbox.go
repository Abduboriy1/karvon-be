package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/campaign"
	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
)

// ListInbox implements GET /inbox.
func (s *Server) ListInbox(w http.ResponseWriter, r *http.Request, params gen.ListInboxParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.InboxFilter{
		Unread:             params.Unread,
		CampaignID:         params.CampaignId,
		Q:                  params.Q,
		LatestOfThread:     params.LatestOfThread == nil || *params.LatestOfThread,
		IncludeAutoReplies: params.IncludeAutoReplies != nil && *params.IncludeAutoReplies,
	}
	if params.Account != nil {
		filter.Accounts = compact(*params.Account)
	}

	sort, err := resolveSortParam(params.Sort, db.InboxSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.ListInbox(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	unread, err := s.campaigns.InboxUnreadCount(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.InboxEmail, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIInboxEmail(row))
	}
	writeJSON(w, r, http.StatusOK, gen.InboxEmailList{
		Data:        data,
		Meta:        pageMeta(page, perPage, result.Total),
		UnreadTotal: unread,
	})
}

// SyncInbox implements POST /inbox/sync.
func (s *Server) SyncInbox(w http.ResponseWriter, r *http.Request) {
	if err := s.campaigns.SyncInbox(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the sync is queued, not done.
	writeJSON(w, r, http.StatusAccepted, nil)
}

// GetInboxThread implements GET /inbox/threads/{threadId}.
func (s *Server) GetInboxThread(w http.ResponseWriter, r *http.Request, threadID gen.ThreadIdPath, params gen.GetInboxThreadParams) {
	thread, err := s.campaigns.GetThread(r.Context(), threadID, params.Refresh != nil && *params.Refresh)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIInboxThread(thread))
}

// MarkInboxThreadRead implements POST /inbox/threads/{threadId}/read.
func (s *Server) MarkInboxThreadRead(w http.ResponseWriter, r *http.Request, threadID gen.ThreadIdPath) {
	if err := s.campaigns.MarkThreadRead(r.Context(), threadID); err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusNoContent, nil)
}

// ListOutreachLeads implements GET /outreach/leads.
func (s *Server) ListOutreachLeads(w http.ResponseWriter, r *http.Request, params gen.ListOutreachLeadsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	sort, err := resolveSortParam(params.Sort, db.OutreachSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	filter := outreachFilter(params.Outcome, params.CampaignId, params.Q, params.IncludeNotContacted)

	result, err := s.campaigns.ListOutreach(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.OutreachLead, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIOutreachLead(row))
	}
	writeJSON(w, r, http.StatusOK, gen.OutreachLeadList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
		Outcomes: gen.OutreachOutcomeCounts{
			Successful:   result.Outcomes[campaign.OutcomeSuccessful],
			Potential:    result.Outcomes[campaign.OutcomePotential],
			Bad:          result.Outcomes[campaign.OutcomeBad],
			Replied:      result.Outcomes[campaign.OutcomeReplied],
			NoReply:      result.Outcomes[campaign.OutcomeNoReply],
			Bounced:      result.Outcomes[campaign.OutcomeBounced],
			NotContacted: result.Outcomes[campaign.OutcomeNotContacted],
		},
	})
}

// ExportOutreachLeads implements GET /outreach/leads/export.csv. Like the business
// export, the body starts before the query finishes, so a failure part-way through
// can only be logged.
func (s *Server) ExportOutreachLeads(w http.ResponseWriter, r *http.Request, params gen.ExportOutreachLeadsParams) {
	sort, err := resolveSortParam(params.Sort, db.OutreachSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	filter := outreachFilter(params.Outcome, params.CampaignId, params.Q, params.IncludeNotContacted)

	filename := fmt.Sprintf("karvon-outreach-%s.csv", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	var flush func()
	if flusher, ok := w.(http.Flusher); ok {
		flush = flusher.Flush
	}
	rows, err := s.campaigns.ExportOutreach(r.Context(), filter, sort, w, flush)
	if err != nil {
		s.log.Error("outreach csv export failed mid-stream", "error", err, "rows_written", rows)
		return
	}
	s.log.Info("outreach csv export finished", "rows", rows, "filename", filename)
}

func outreachFilter(outcomes *gen.OutreachOutcomeFilter, campaignID *uuid.UUID, q *string, includeNotContacted *bool) db.OutreachFilter {
	filter := db.OutreachFilter{CampaignID: campaignID, Q: q, IncludeNotContacted: includeNotContacted != nil && *includeNotContacted}
	if outcomes != nil {
		for _, o := range *outcomes {
			filter.Outcomes = append(filter.Outcomes, string(o))
		}
	}
	return filter
}

/* ---------------------------------------------------------------- mappers */

func toAPIInboxEmail(row db.InboxRow) gen.InboxEmail {
	out := gen.InboxEmail{
		Id: row.ID, ThreadId: row.ThreadID, Direction: gen.InboxEmailDirection(row.Direction),
		EmailAccount: row.EmailAccount, LeadEmail: row.LeadEmail, FromAddress: row.FromAddress,
		ToAddresses: row.ToAddresses, Subject: row.Subject, ContentPreview: row.ContentPreview,
		IsUnread: row.IsUnread, IsAutoReply: row.IsAutoReply, InterestStatus: widenInt32(row.InterestStatus),
		AiInterestValue: row.AIInterestValue, InstantlyCampaignId: row.InstantlyCampaignID,
		CampaignId: nullUUID(row.CampaignID), CampaignName: row.CampaignName, ContactId: nullUUID(row.ContactID),
		FirstName: row.FirstName, LastName: row.LastName, Company: row.Company,
		CampaignLeadId: nullUUID(row.CampaignLeadID), SentAt: row.SentAt.UTC(), ThreadEmails: row.ThreadEmails,
	}
	if row.LeadInterest != nil {
		out.LeadInterestLabel = optString(campaignsvc.InterestLabelOf(nil, row.LeadInterest))
	}
	if row.LeadOutcome != nil {
		outcome := gen.OutreachOutcome(*row.LeadOutcome)
		out.LeadOutcome = &outcome
	}
	return out
}

func toAPIInboxThread(in campaignsvc.Thread) gen.InboxThread {
	emails := make([]gen.InboxMessage, 0, len(in.Emails))
	for _, e := range in.Emails {
		emails = append(emails, toAPIInboxMessage(e))
	}
	return gen.InboxThread{ThreadId: in.ThreadID, Stale: in.Stale, Emails: emails}
}

func toAPIInboxMessage(row dbgen.InboxEmail) gen.InboxMessage {
	return gen.InboxMessage{
		Id: row.ID, ThreadId: row.ThreadID, MessageId: row.MessageID, Direction: gen.InboxMessageDirection(row.Direction),
		EmailAccount: row.EmailAccount, LeadEmail: row.LeadEmail, FromAddress: row.FromAddress,
		ToAddresses: row.ToAddresses, CcAddresses: row.CcAddresses, Subject: row.Subject, BodyText: row.BodyText,
		BodyHtml: row.BodyHtml, ContentPreview: row.ContentPreview, Step: row.Step, IsUnread: row.IsUnread,
		IsAutoReply: row.IsAutoReply, InterestStatus: widenInt32(row.InterestStatus),
		CampaignId: nullUUID(row.CampaignID), ContactId: nullUUID(row.ContactID), SentAt: row.SentAt.UTC(),
	}
}

func toAPIOutreachLead(row db.OutreachRow) gen.OutreachLead {
	return gen.OutreachLead{
		LeadId: row.LeadID, CampaignId: row.CampaignID, CampaignName: row.CampaignName, ContactId: row.ContactID,
		Email: row.Email, FirstName: row.FirstName, LastName: row.LastName, Company: row.Company, Title: row.Title,
		Phone: row.Phone, Website: row.Website, Status: gen.CampaignLeadStatus(row.Status),
		InterestStatus: widenInt32(row.InterestStatus),
		InterestLabel:  optString(campaignsvc.InterestLabelOf(row.InterestLabel, row.InterestStatus)),
		Outcome:        gen.OutreachOutcome(row.Outcome), SendingAccount: row.SendingAccount, EmailsSent: row.EmailsSent,
		FirstContactedAt: utcPtr(row.FirstContactedAt), LastContactedAt: utcPtr(row.LastContactedAt),
		OpenCount: int(row.OpenCount), ClickCount: int(row.ClickCount), ReplyCount: int(row.ReplyCount),
		LastRepliedAt: utcPtr(row.LastRepliedAt), LastReplySubject: row.LastReplySubject,
		LastReplyPreview: row.LastReplyPreview, LastReplyThreadId: row.LastReplyThread,
	}
}
