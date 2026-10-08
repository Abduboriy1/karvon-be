package stats

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
)

var reportNow = time.Date(2026, 10, 8, 15, 30, 0, 0, time.UTC)

func TestResolveRangeDefaultsToTheLast30Days(t *testing.T) {
	rng, err := ResolveRange(ReportInput{}, reportNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := rng.From.Format(time.DateOnly); got != "2026-09-09" {
		t.Errorf("from = %s, want 2026-09-09", got)
	}
	if got := rng.To.Format(time.DateOnly); got != "2026-10-08" {
		t.Errorf("to = %s, want 2026-10-08", got)
	}
	if rng.Days != 30 || rng.Bucket != BucketDay || rng.TZ != "UTC" {
		t.Errorf("days, bucket, tz = %d, %s, %s", rng.Days, rng.Bucket, rng.TZ)
	}
	if !rng.End.Equal(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %s, want the midnight after to", rng.End)
	}
	if rng.PrevFrom.Format(time.DateOnly) != "2026-08-10" || rng.PrevTo.Format(time.DateOnly) != "2026-09-08" {
		t.Errorf("previous = %s..%s", rng.PrevFrom, rng.PrevTo)
	}
}

// The window is cut on local midnights, so a client in another zone sees their days.
func TestResolveRangeCutsDaysInTheRequestedZone(t *testing.T) {
	in := ReportInput{
		From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		TZ:   "America/New_York",
	}
	rng, err := ResolveRange(in, reportNow)
	if err != nil {
		t.Fatal(err)
	}
	// 31 calendar days although DST starts on 8 March and one of them is 23 hours.
	if rng.Days != 31 {
		t.Errorf("days = %d, want 31", rng.Days)
	}
	if got := rng.Start.UTC().Format(time.RFC3339); got != "2026-03-01T05:00:00Z" {
		t.Errorf("start = %s", got)
	}
	if got := rng.End.UTC().Format(time.RFC3339); got != "2026-04-01T04:00:00Z" {
		t.Errorf("end = %s", got)
	}
}

func TestResolveRangePicksTheBucketByLength(t *testing.T) {
	for _, tc := range []struct {
		days int
		want string
	}{{1, BucketDay}, {62, BucketDay}, {63, BucketWeek}, {182, BucketWeek}, {183, BucketMonth}, {731, BucketMonth}} {
		to := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
		rng, err := ResolveRange(ReportInput{From: to.AddDate(0, 0, -(tc.days - 1)), To: to}, reportNow)
		if err != nil {
			t.Fatalf("%d days: %v", tc.days, err)
		}
		if rng.Bucket != tc.want {
			t.Errorf("%d days: bucket = %s, want %s", tc.days, rng.Bucket, tc.want)
		}
	}
}

func TestResolveRangeRejectsBadInput(t *testing.T) {
	to := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for name, in := range map[string]ReportInput{
		"tz":            {TZ: "Mars/Olympus"},
		"bucket":        {Bucket: "year"},
		"reversed":      {From: to, To: to.AddDate(0, 0, -1)},
		"too long":      {From: to.AddDate(0, 0, -731), To: to},
		"daily too far": {From: to.AddDate(0, 0, -450), To: to, Bucket: BucketDay},
	} {
		_, err := ResolveRange(in, reportNow)
		var appErr *apperr.Error
		if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
}

func TestLeadCostPricesEveryStepAndShowsTheWorking(t *testing.T) {
	recurring := RecurringCosts{MailboxMonthlyCents: 600, FixedMonthlyCents: 9700}
	in := db.DashboardSummary{
		Businesses:        1000,
		EmailsFound:       800,
		EmailsGreen:       400,
		PaidChecks:        500,
		ScrapingCents:     4000,    // $40.00
		VerificationCents: 2500,    // $25.00
		DomainsCents:      1200,    // $12.00
		MailboxDays:       304.375, // 10 mailbox-months → $60.00
		ActiveDays:        30.4375, // one month → $97.00
		LeadsContacted:    350,
		Replied:           20,
		SuccessfulLeads:   7,
	}

	costs := recurring.price(in)
	if costs.MailboxesCents != 6000 || costs.ToolsCents != 9700 {
		t.Fatalf("mailboxes, tools = %d, %d", costs.MailboxesCents, costs.ToolsCents)
	}
	if costs.TotalCents != 23400 {
		t.Fatalf("total = %d, want 23400", costs.TotalCents)
	}

	lc := recurring.leadCost(in, costs, false)
	if lc.CostPerSuccessfulLeadCents == nil || *lc.CostPerSuccessfulLeadCents != 3342.86 {
		t.Fatalf("cost per successful lead = %v, want 3342.86 ($234.00 / 7)", lc.CostPerSuccessfulLeadCents)
	}
	if lc.Formula != "$234.00 total spend ÷ 7 successful leads = $33.43 each" {
		t.Errorf("formula = %q", lc.Formula)
	}

	want := map[string]struct {
		cost int64
		per  float64
	}{
		StepBusinesses: {4000, 4},        // $40 / 1000
		StepEmails:     {4000, 5},        // $40 / 800
		StepVerified:   {6500, 16.25},    // ($40 + $25) / 400
		StepContacted:  {23400, 66.86},   // $234 / 350
		StepReplied:    {23400, 1170},    // $234 / 20
		StepSuccessful: {23400, 3342.86}, // $234 / 7
	}
	if len(lc.Steps) != len(want) {
		t.Fatalf("steps = %d", len(lc.Steps))
	}
	for _, st := range lc.Steps {
		w := want[st.Key]
		if st.CostCents != w.cost || st.CostPerUnitCents == nil || *st.CostPerUnitCents != w.per {
			t.Errorf("%s: cost %d per %v, want %d per %v", st.Key, st.CostCents, st.CostPerUnitCents, w.cost, w.per)
		}
	}
	if lc.Steps[0].RateFromPrevious != nil {
		t.Error("the first step has no previous step")
	}
	if r := lc.Steps[1].RateFromPrevious; r == nil || *r != 0.8 {
		t.Errorf("emails / businesses = %v, want 0.8", r)
	}
	if !strings.Contains(lc.Steps[2].Formula, "($40.00 lead data + $25.00 verification) = $65.00 ÷ 400 verified addresses = $0.1625 each") {
		t.Errorf("verified formula = %q", lc.Steps[2].Formula)
	}
	if !strings.HasPrefix(lc.Notes[0], "Total spend $234.00 = $40.00 Lead data") {
		t.Errorf("first note = %q", lc.Notes[0])
	}
}

func TestLeadCostWithNoSuccessHasNoPerLeadFigure(t *testing.T) {
	in := db.DashboardSummary{ScrapingCents: 1000, Businesses: 10}
	var recurring RecurringCosts
	lc := recurring.leadCost(in, recurring.price(in), true)
	if lc.CostPerSuccessfulLeadCents != nil {
		t.Errorf("cost per lead = %v, want nil", *lc.CostPerSuccessfulLeadCents)
	}
	if !strings.HasSuffix(lc.Formula, "none yet, so no cost per unit") {
		t.Errorf("formula = %q", lc.Formula)
	}
	for _, n := range lc.Notes {
		if strings.Contains(n, "inside this period") {
			t.Error("the all-time figure should not carry the period caveat")
		}
	}
}

func TestUSDUnitKeepsFractionsOfACent(t *testing.T) {
	for cents, want := range map[float64]string{0.32: "$0.0032", 16.25: "$0.1625", 3342.86: "$33.43"} {
		if got := usdUnit(cents); got != want {
			t.Errorf("usdUnit(%v) = %s, want %s", cents, got, want)
		}
	}
}

func TestUSD(t *testing.T) {
	for cents, want := range map[int64]string{0: "$0.00", 5: "$0.05", 123456789: "$1,234,567.89", -250: "-$2.50"} {
		if got := usd(cents); got != want {
			t.Errorf("usd(%d) = %s, want %s", cents, got, want)
		}
	}
}
