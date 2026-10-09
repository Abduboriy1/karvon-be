package integration_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
)

type dashboardImportedPayload struct {
	Campaigns      int64   `json:"campaigns"`
	Sends          int64   `json:"sends"`
	LeadsContacted int64   `json:"leads_contacted"`
	Opened         int64   `json:"opened"`
	Clicked        int64   `json:"clicked"`
	Replied        int64   `json:"replied"`
	Interested     int64   `json:"interested"`
	ReplyRate      float64 `json:"reply_rate"`
	InterestedRate float64 `json:"interested_rate"`
}

type dashboardSummaryPayload struct {
	Outreach struct {
		Sends           int64                    `json:"sends"`
		Replied         int64                    `json:"replied"`
		Interested      int64                    `json:"interested"`
		SuccessfulLeads int64                    `json:"successful_leads"`
		Imported        dashboardImportedPayload `json:"imported"`
	} `json:"outreach"`
	LeadCost struct {
		SuccessfulLeads         int64    `json:"successful_leads"`
		ImportedSuccessfulLeads int64    `json:"imported_successful_leads"`
		Notes                   []string `json:"notes"`
		Steps                   []struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		} `json:"steps"`
	} `json:"lead_cost"`
}

type dashboardReportPayload struct {
	Current  dashboardSummaryPayload `json:"current"`
	Previous dashboardSummaryPayload `json:"previous"`
	AllTime  dashboardSummaryPayload `json:"all_time"`
	Series   []struct {
		BucketStart        string `json:"bucket_start"`
		Sends              int64  `json:"sends"`
		ImportedSends      int64  `json:"imported_sends"`
		ImportedReplied    int64  `json:"imported_replied"`
		ImportedInterested int64  `json:"imported_interested"`
	} `json:"series"`
	Campaigns []struct {
		Name            string `json:"name"`
		Source          string `json:"source"`
		Sends           int64  `json:"sends"`
		Replied         int64  `json:"replied"`
		PositiveReplies int64  `json:"positive_replies"`
		Bounced         *int64 `json:"bounced"`
	} `json:"campaigns"`
}

// A campaign started in Instantly sends nothing through Karvon, and its analytics
// snapshots are lifetime totals, so the dashboard reads its per-day figures: they
// fall in the window by calendar day, sit beside our own outreach figures without
// leaking into them, and appear in the series and the campaign table. The lead
// cost counts them, since the spend pays for them too: its opportunities are
// successful leads. The campaign has completed, which must not stop it syncing:
// its history is fetched all the same.
func TestTheDashboardWindowsAnImportedCampaignByDay(t *testing.T) {
	c := newCampaignHarness(t, 1)
	today := c.now.Now()

	c.instantly.PutDaily("native-9",
		instantly.DailyAnalytics{Date: "2026-02-20", Sent: 40, NewLeadsContacted: 20, UniqueOpened: 10, Replies: 2, UniqueReplies: 2, UniqueClicks: 1, Opportunities: 1},
		instantly.DailyAnalytics{Date: "2026-02-25", Sent: 50, NewLeadsContacted: 25, UniqueOpened: 20, Replies: 3, UniqueReplies: 3, UniqueClicks: 2, Opportunities: 1},
		instantly.DailyAnalytics{Date: "2026-03-01", Sent: 30, NewLeadsContacted: 10, UniqueOpened: 12, Replies: 2, UniqueReplies: 2, UniqueClicks: 1, Opportunities: 1},
		instantly.DailyAnalytics{Date: "2026-03-02", Sent: 20, NewLeadsContacted: 5, UniqueOpened: 8, Replies: 1, UniqueReplies: 1},
	)
	c.instantly.PutCampaign(instantly.Campaign{
		ID: "native-9", Name: "Denver roofers", Status: instantly.CampaignStatusCompleted,
		Sequences:        []instantly.Sequence{{Steps: []instantly.Step{{Type: "email"}}}},
		TimestampCreated: today.AddDate(0, 0, -30),
	}, instantly.CampaignAnalytics{LeadsCount: 80, ContactedCount: 60, EmailsSentCount: 140, ReplyCountUnique: 8, TotalOpportunities: 3})
	c.waitForImported("native-9", func(p sourcedCampaignPayload) bool { return p.SendsTotal == 140 })

	// The daily rows land in the same sync, just after the snapshot the list reads.
	var report dashboardReportPayload
	deadline := time.Now().Add(30 * time.Second)
	for {
		decodeInto(t, c.mustRequest(http.MethodGet, "/api/v1/dashboard/report?from=2026-02-24&to=2026-03-02&tz=UTC", "", http.StatusOK), &report)
		if report.Current.Outreach.Imported.Sends > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cur := report.Current.Outreach
	if cur.Sends != 0 || cur.Replied != 0 || cur.Interested != 0 || cur.SuccessfulLeads != 0 {
		t.Fatalf("the import leaked into our own outreach: %+v", cur)
	}
	want := dashboardImportedPayload{
		Campaigns: 1, Sends: 100, LeadsContacted: 40, Opened: 40, Clicked: 3, Replied: 6, Interested: 2,
		ReplyRate: 0.06, InterestedRate: 0.02,
	}
	if cur.Imported != want {
		t.Fatalf("imported = %+v, want %+v", cur.Imported, want)
	}
	if prev := report.Previous.Outreach.Imported; prev.Sends != 40 || prev.Interested != 1 {
		t.Fatalf("the previous window's imported figures = %+v, want the 20 Feb day", prev)
	}
	if all := report.AllTime.Outreach.Imported; all.Sends != 140 || all.Interested != 3 {
		t.Fatalf("the all-time imported figures = %+v", all)
	}
	lc := report.Current.LeadCost
	if lc.SuccessfulLeads != 2 || lc.ImportedSuccessfulLeads != 2 {
		t.Fatalf("lead cost successful leads = %d (imported %d), want Instantly's 2 opportunities", lc.SuccessfulLeads, lc.ImportedSuccessfulLeads)
	}
	steps := map[string]int64{}
	for _, st := range lc.Steps {
		steps[st.Key] = st.Count
	}
	if steps["leads_contacted"] != 40 || steps["replied"] != 6 || steps["successful_leads"] != 2 {
		t.Fatalf("lead cost steps = %v, want the imported outreach counted", steps)
	}
	if notes := strings.Join(lc.Notes, " "); !strings.Contains(notes, "40 leads emailed, 6 replies and 2 opportunities") {
		t.Fatalf("the lead cost does not say what it took from Instantly: %q", notes)
	}

	byDay := map[string]int64{}
	var interested int64
	for _, p := range report.Series {
		if p.Sends != 0 {
			t.Fatalf("the series counts the import as our own sends on %s", p.BucketStart)
		}
		byDay[p.BucketStart] = p.ImportedSends
		interested += p.ImportedInterested
	}
	if byDay["2026-02-25"] != 50 || byDay["2026-03-01"] != 30 || byDay["2026-03-02"] != 20 || interested != 2 {
		t.Fatalf("imported series = %v (interested %d)", byDay, interested)
	}

	if len(report.Campaigns) != 1 {
		t.Fatalf("campaigns = %+v, want only the imported one", report.Campaigns)
	}
	row := report.Campaigns[0]
	if row.Name != "Denver roofers" || row.Source != "instantly" || row.Sends != 100 || row.Replied != 6 ||
		row.PositiveReplies != 2 || row.Bounced != nil {
		t.Fatalf("the imported campaign row = %+v", row)
	}

	// The days are calendar days, so a zone far from UTC still reads 1 March as 1 March.
	decodeInto(t, c.mustRequest(http.MethodGet, "/api/v1/dashboard/report?from=2026-03-01&to=2026-03-01&tz=America/Los_Angeles", "", http.StatusOK), &report)
	if got := report.Current.Outreach.Imported; got.Sends != 30 || got.Interested != 1 {
		t.Fatalf("1 March in Los Angeles = %+v, want the 1 March day", got)
	}
}
