package httpapi

import (
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/stats"
)

// GetDashboardReport implements GET /dashboard/report.
func (s *Server) GetDashboardReport(w http.ResponseWriter, r *http.Request, params gen.GetDashboardReportParams) {
	var in stats.ReportInput
	if params.From != nil {
		in.From = params.From.Time
	}
	if params.To != nil {
		in.To = params.To.Time
	}
	if params.Tz != nil {
		in.TZ = *params.Tz
	}
	if params.Bucket != nil {
		in.Bucket = string(*params.Bucket)
	}

	report, err := s.stats.Report(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIDashboardReport(report))
}

func toAPIDashboardReport(in stats.Report) gen.DashboardReport {
	series := make([]gen.DashboardPoint, 0, len(in.Series))
	for _, p := range in.Series {
		series = append(series, gen.DashboardPoint{
			BucketStart:       civilDate(p.BucketStart),
			Businesses:        p.Businesses,
			EmailsFound:       p.EmailsFound,
			EmailsGreen:       p.EmailsGreen,
			Sends:             p.Sends,
			Replied:           p.Replied,
			PositiveReplies:   p.PositiveReplies,
			SuccessfulLeads:   p.SuccessfulLeads,
			ScrapingCents:     p.ScrapingCents,
			VerificationCents: p.VerificationCents,
			DomainsCents:      p.DomainsCents,
			MailboxesCents:    p.MailboxesCents,
			ToolsCents:        p.ToolsCents,
			CostCents:         p.CostCents,
		})
	}

	campaigns := make([]gen.DashboardCampaign, 0, len(in.Campaigns))
	for _, c := range in.Campaigns {
		campaigns = append(campaigns, gen.DashboardCampaign{
			Id:              c.ID,
			Name:            c.Name,
			Status:          c.Status,
			LaunchedAt:      c.LaunchedAt,
			LeadsTotal:      c.LeadsTotal,
			Sends:           c.Sends,
			LeadsContacted:  c.LeadsContacted,
			Opened:          c.Opened,
			Replied:         c.Replied,
			PositiveReplies: c.PositiveReplies,
			Bounced:         c.Bounced,
			OpenRate:        sendRate(c.Opened, c.Sends),
			ReplyRate:       sendRate(c.Replied, c.Sends),
		})
	}

	return gen.DashboardReport{
		Range: gen.DashboardRange{
			From:         civilDate(in.Range.From),
			To:           civilDate(in.Range.To),
			Days:         in.Range.Days,
			Tz:           in.Range.TZ,
			Bucket:       gen.DashboardBucket(in.Range.Bucket),
			PreviousFrom: civilDate(in.Range.PrevFrom),
			PreviousTo:   civilDate(in.Range.PrevTo),
		},
		GeneratedAt: in.GeneratedAt,
		Current:     toAPIDashboardSummary(in.Current),
		Previous:    toAPIDashboardSummary(in.Previous),
		AllTime:     toAPIDashboardSummary(in.AllTime),
		Series:      series,
		Campaigns:   campaigns,
		Breakdowns: gen.DashboardBreakdowns{
			Categories:      toAPIDashboardCounts(in.Breakdowns.Categories),
			States:          toAPIDashboardCounts(in.Breakdowns.States),
			VerificationMix: toAPIDashboardCounts(in.Breakdowns.VerificationMix),
			CampaignStatus:  toAPIDashboardCounts(in.Breakdowns.CampaignStatus),
		},
	}
}

func toAPIDashboardSummary(in stats.Summary) gen.DashboardSummary {
	lines := make([]gen.DashboardCostLine, 0, len(in.Costs.Lines))
	for _, l := range in.Costs.Lines {
		lines = append(lines, gen.DashboardCostLine{
			Key: gen.DashboardCostLineKey(l.Key), Label: l.Label, Cents: l.Cents, Basis: l.Basis,
		})
	}
	steps := make([]gen.DashboardLeadCostStep, 0, len(in.LeadCost.Steps))
	for _, st := range in.LeadCost.Steps {
		steps = append(steps, gen.DashboardLeadCostStep{
			Key:              gen.DashboardLeadCostStepKey(st.Key),
			Label:            st.Label,
			Count:            st.Count,
			CostCents:        st.CostCents,
			CostPerUnitCents: st.CostPerUnitCents,
			RateFromPrevious: st.RateFromPrevious,
			Formula:          st.Formula,
		})
	}
	notes := in.LeadCost.Notes
	if notes == nil {
		notes = []string{}
	}

	return gen.DashboardSummary{
		Costs: gen.DashboardCosts{
			TotalCents:        in.Costs.TotalCents,
			ScrapingCents:     in.Costs.ScrapingCents,
			VerificationCents: in.Costs.VerificationCents,
			DomainsCents:      in.Costs.DomainsCents,
			MailboxesCents:    in.Costs.MailboxesCents,
			ToolsCents:        in.Costs.ToolsCents,
			Lines:             lines,
		},
		LeadCost: gen.DashboardLeadCost{
			SuccessfulLeads:            in.LeadCost.SuccessfulLeads,
			CostPerSuccessfulLeadCents: in.LeadCost.CostPerSuccessfulLeadCents,
			Definition:                 in.LeadCost.Definition,
			Formula:                    in.LeadCost.Formula,
			Steps:                      steps,
			Notes:                      notes,
		},
		Leads: gen.DashboardLeads{
			Businesses:          in.Businesses,
			BusinessesWithEmail: in.BusinessesWithEmail,
			EmailFindRate:       in.EmailFindRate,
			EmailsFound:         in.EmailsFound,
			EmailsVerified:      in.EmailsVerified,
			EmailsGreen:         in.EmailsGreen,
			PaidChecks:          in.PaidChecks,
		},
		Outreach: gen.DashboardOutreach{
			CampaignsLaunched: in.CampaignsLaunched,
			Sends:             in.Sends,
			LeadsContacted:    in.LeadsContacted,
			Opened:            in.Opened,
			Clicked:           in.Clicked,
			Replied:           in.Replied,
			PositiveReplies:   in.PositiveReplies,
			Bounced:           in.Bounced,
			Unsubscribed:      in.Unsubscribed,
			Interested:        in.Interested,
			MeetingsBooked:    in.MeetingsBooked,
			SuccessfulLeads:   in.SuccessfulLeads,
			OpenRate:          in.OpenRate,
			ReplyRate:         in.ReplyRate,
			PositiveReplyRate: in.PositiveReplyRate,
			BounceRate:        in.BounceRate,
		},
	}
}

func toAPIDashboardCounts(in []db.DashboardCount) []gen.DashboardCount {
	out := make([]gen.DashboardCount, 0, len(in))
	for _, c := range in {
		out = append(out, gen.DashboardCount{Label: c.Label, Count: c.Count})
	}
	return out
}

func civilDate(t time.Time) openapi_types.Date { return openapi_types.Date{Time: t} }

// sendRate is a/b to four decimal places, 0 when b is 0.
func sendRate(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(int64(float64(a)/float64(b)*10000+0.5)) / 10000
}
