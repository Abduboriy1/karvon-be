package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/stats"
)

func TestDashboardReportForwardsTheWindowAndShapesThePayload(t *testing.T) {
	handler, deps := newTestServer(t)
	per := 3342.86
	deps.stats.report = stats.Report{
		Current: stats.Summary{
			DashboardSummary: db.DashboardSummary{Businesses: 1000, SuccessfulLeads: 7},
			Costs: stats.Costs{TotalCents: 23400, Lines: []stats.CostLine{
				{Key: stats.CostScraping, Label: "Lead data", Cents: 4000, Basis: "providers"},
			}},
			LeadCost: stats.LeadCost{
				SuccessfulLeads:            7,
				CostPerSuccessfulLeadCents: &per,
				Formula:                    "$234.00 total spend ÷ 7 successful leads = $33.43 each",
				Steps: []stats.LeadCostStep{
					{Key: stats.StepBusinesses, Label: "Businesses scraped", Count: 1000, CostCents: 4000},
				},
			},
		},
		Series: []stats.Point{{CostCents: 10}},
	}

	rec := do(t, handler, http.MethodGet,
		"/api/v1/dashboard/report?from=2026-09-01&to=2026-09-30&tz=Europe/London&bucket=week", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if in := deps.stats.input; in.From.Format("2006-01-02") != "2026-09-01" ||
		in.To.Format("2006-01-02") != "2026-09-30" || in.TZ != "Europe/London" || in.Bucket != "week" {
		t.Errorf("input = %+v", in)
	}

	payload := decodeJSONBody[struct {
		Range struct {
			From, To, Bucket string
			Days             int
		} `json:"range"`
		Current struct {
			Costs struct {
				TotalCents int64 `json:"total_cents"`
				Lines      []struct {
					Key, Basis string
				} `json:"lines"`
			} `json:"costs"`
			LeadCost struct {
				CostPerSuccessfulLeadCents *float64 `json:"cost_per_successful_lead_cents"`
				Formula                    string   `json:"formula"`
				Notes                      []string
				Steps                      []struct {
					Key              string   `json:"key"`
					CostPerUnitCents *float64 `json:"cost_per_unit_cents"`
					RateFromPrevious *float64 `json:"rate_from_previous"`
				} `json:"steps"`
			} `json:"lead_cost"`
		} `json:"current"`
		Previous struct {
			LeadCost struct {
				CostPerSuccessfulLeadCents *float64 `json:"cost_per_successful_lead_cents"`
				Notes                      []string
			} `json:"lead_cost"`
		} `json:"previous"`
		Series []struct {
			CostCents int64 `json:"cost_cents"`
		} `json:"series"`
	}](t, rec)

	if payload.Range.From != "2026-09-01" || payload.Range.To != "2026-09-30" || payload.Range.Days != 30 || payload.Range.Bucket != "week" {
		t.Errorf("range = %+v", payload.Range)
	}
	lc := payload.Current.LeadCost
	if lc.CostPerSuccessfulLeadCents == nil || *lc.CostPerSuccessfulLeadCents != 3342.86 || lc.Formula == "" {
		t.Errorf("lead cost = %+v", lc)
	}
	if len(lc.Steps) != 1 || lc.Steps[0].Key != "businesses" || lc.Steps[0].CostPerUnitCents != nil {
		t.Errorf("steps = %+v", lc.Steps)
	}
	// An empty window still answers with null, and an array where the client loops.
	if payload.Previous.LeadCost.CostPerSuccessfulLeadCents != nil || payload.Previous.LeadCost.Notes == nil {
		t.Errorf("previous = %+v", payload.Previous.LeadCost)
	}
	if payload.Current.Costs.TotalCents != 23400 || len(payload.Current.Costs.Lines) != 1 || len(payload.Series) != 1 {
		t.Errorf("costs, series = %+v, %+v", payload.Current.Costs, payload.Series)
	}
}

func TestDashboardReportRejectsAnUnknownZone(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/v1/dashboard/report?tz=Mars/Olympus", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}
