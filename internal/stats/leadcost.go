package stats

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bory/karvon-be/internal/db"
)

// DaysPerMonth turns a monthly price into a daily one (365.25 / 12).
const DaysPerMonth = 30.4375

// RecurringCosts are the monthly bills no table records. The report prorates them
// by the day over the time it covers.
type RecurringCosts struct {
	// MailboxMonthlyCents is the price of one provisioned Workspace mailbox.
	MailboxMonthlyCents int64
	// FixedMonthlyCents is the flat tooling bill: sending platform, newsletter, etc.
	FixedMonthlyCents int64
}

func (r RecurringCosts) mailboxCents(mailboxDays float64) int64 {
	return int64(math.Round(mailboxDays * float64(r.MailboxMonthlyCents) / DaysPerMonth))
}

func (r RecurringCosts) toolsCents(activeDays float64) int64 {
	return int64(math.Round(activeDays * float64(r.FixedMonthlyCents) / DaysPerMonth))
}

// Cost line keys, stable for the client to colour and order by.
const (
	CostScraping     = "scraping"
	CostVerification = "verification"
	CostDomains      = "domains"
	CostMailboxes    = "mailboxes"
	CostTools        = "tools"
)

// CostLine is one source of spend and what it was worked out from.
type CostLine struct {
	Key   string
	Label string
	Cents int64
	// Basis says how the figure was reached, in words a client can follow.
	Basis string
}

// Costs is a window's spend, split by where it went.
type Costs struct {
	ScrapingCents     int64
	VerificationCents int64
	DomainsCents      int64
	MailboxesCents    int64
	ToolsCents        int64
	TotalCents        int64
	Lines             []CostLine
}

func (r RecurringCosts) price(in db.DashboardSummary) Costs {
	c := Costs{
		ScrapingCents:     in.ScrapingCents,
		VerificationCents: in.VerificationCents,
		DomainsCents:      in.DomainsCents,
		MailboxesCents:    r.mailboxCents(in.MailboxDays),
		ToolsCents:        r.toolsCents(in.ActiveDays),
	}
	c.TotalCents = c.ScrapingCents + c.VerificationCents + c.DomainsCents + c.MailboxesCents + c.ToolsCents

	mailboxBasis := "Not configured (KARVON_REPORT_MAILBOX_MONTHLY_CENTS)"
	if r.MailboxMonthlyCents > 0 {
		mailboxBasis = fmt.Sprintf("%s mailbox-days × %s per mailbox per month ÷ %.2f days",
			days(in.MailboxDays), usd(r.MailboxMonthlyCents), DaysPerMonth)
	}
	toolsBasis := "Not configured (KARVON_REPORT_FIXED_MONTHLY_CENTS)"
	if r.FixedMonthlyCents > 0 {
		toolsBasis = fmt.Sprintf("%s days × %s per month ÷ %.2f days",
			days(in.ActiveDays), usd(r.FixedMonthlyCents), DaysPerMonth)
	}

	c.Lines = []CostLine{
		{Key: CostScraping, Label: "Lead data (Google Maps scraping)", Cents: c.ScrapingCents,
			Basis: "What the data providers charged for the listings their runs returned"},
		{Key: CostVerification, Label: "Email verification", Cents: c.VerificationCents,
			Basis: fmt.Sprintf("%s addresses checked by the paid verifier at its per-1,000 rate", count(in.PaidChecks))},
		{Key: CostDomains, Label: "Sending domains", Cents: c.DomainsCents,
			Basis: "Registration price of the domains bought to send from"},
		{Key: CostMailboxes, Label: "Sending mailboxes", Cents: c.MailboxesCents, Basis: mailboxBasis},
		{Key: CostTools, Label: "Outreach tools", Cents: c.ToolsCents, Basis: toolsBasis},
	}
	return c
}

// Funnel step keys.
const (
	StepBusinesses = "businesses"
	StepEmails     = "emails_found"
	StepVerified   = "emails_verified"
	StepContacted  = "leads_contacted"
	StepReplied    = "replied"
	StepSuccessful = "successful_leads"
)

// LeadCostStep is one stage of the funnel, with the spend charged to reach it.
type LeadCostStep struct {
	Key   string
	Label string
	Count int64
	// CostCents is the spend that stage carries: everything spent to get that far.
	CostCents int64
	// CostPerUnitCents is CostCents over Count, to a hundredth of a cent — a scraped
	// business can cost a fraction of one; nil when Count is 0.
	CostPerUnitCents *float64
	// RateFromPrevious is Count over the previous step's Count; nil on the first step
	// or when the previous step is 0. It can exceed 1 (several addresses per business).
	RateFromPrevious *float64
	// Formula is the arithmetic, with the real figures in it.
	Formula string
}

// LeadCost answers "what does one good lead cost us?" and shows the working.
type LeadCost struct {
	// SuccessfulLeads counts every campaign's: ours, plus the opportunities of the
	// campaigns started in Instantly, which the spend paid for just the same.
	SuccessfulLeads int64
	// ImportedSuccessfulLeads is the part of SuccessfulLeads that is Instantly's
	// opportunity count.
	ImportedSuccessfulLeads int64
	// CostPerSuccessfulLeadCents is total spend over successful leads, to a
	// hundredth of a cent; nil when there were none.
	CostPerSuccessfulLeadCents *float64
	Definition                 string
	Formula                    string
	Steps                      []LeadCostStep
	Notes                      []string
}

// SuccessDefinition is what counts as a successful lead.
const SuccessDefinition = "A contact who replied positively, was marked interested, or booked a meeting " +
	"— or, in a campaign started in Instantly, an opportunity Instantly recorded. " +
	"Each contact counts once, however many of those they did."

func (r RecurringCosts) leadCost(in db.DashboardSummary, c Costs, allTime bool) LeadCost {
	// The outreach steps count every campaign. The spend is one bill — the same
	// mailboxes, tools and lead data feed the campaigns started in Instantly — so
	// leaving their results out would overstate what a lead costs.
	im := in.Imported
	contacted := in.LeadsContacted + im.LeadsContacted
	replied := in.Replied + im.Replied
	successful := in.SuccessfulLeads + im.Interested

	data := c.ScrapingCents
	dataVerified := c.ScrapingCents + c.VerificationCents
	total := c.TotalCents

	steps := []LeadCostStep{
		step(StepBusinesses, "Businesses scraped", in.Businesses, data,
			"%s lead data ÷ %s businesses"),
		step(StepEmails, "Email addresses found", in.EmailsFound, data,
			"%s lead data ÷ %s addresses found"),
		step(StepVerified, "Verified deliverable addresses", in.EmailsGreen, dataVerified,
			"("+usd(c.ScrapingCents)+" lead data + "+usd(c.VerificationCents)+" verification) = %s ÷ %s verified addresses"),
		step(StepContacted, "Leads emailed", contacted, total,
			"%s total spend ÷ %s leads emailed"),
		step(StepReplied, "Replies received", replied, total,
			"%s total spend ÷ %s replies"),
		step(StepSuccessful, "Successful leads", successful, total,
			"%s total spend ÷ %s successful leads"),
	}
	for i := 1; i < len(steps); i++ {
		if prev := steps[i-1].Count; prev > 0 {
			rate := ratio(steps[i].Count, prev)
			steps[i].RateFromPrevious = &rate
		}
	}

	out := LeadCost{
		SuccessfulLeads:            successful,
		ImportedSuccessfulLeads:    im.Interested,
		CostPerSuccessfulLeadCents: steps[len(steps)-1].CostPerUnitCents,
		Definition:                 SuccessDefinition,
		Formula:                    steps[len(steps)-1].Formula,
		Steps:                      steps,
	}

	totalFormula := make([]string, 0, len(c.Lines))
	for _, line := range c.Lines {
		if line.Cents > 0 {
			totalFormula = append(totalFormula, usd(line.Cents)+" "+line.Label)
		}
	}
	if len(totalFormula) > 1 {
		out.Notes = append(out.Notes, "Total spend "+usd(total)+" = "+strings.Join(totalFormula, " + ")+".")
	}
	if !allTime {
		out.Notes = append(out.Notes, "Spend and results are both counted inside this period. A lead that "+
			"answers this period may have been bought in an earlier one, so short periods swing; "+
			"the all-time figure is the steadiest.")
	}
	if r.MailboxMonthlyCents == 0 || r.FixedMonthlyCents == 0 {
		out.Notes = append(out.Notes, "Mailbox and tooling subscriptions are only included once their monthly "+
			"prices are configured.")
	}
	if im.Sends > 0 || im.Interested > 0 {
		out.Notes = append(out.Notes, "Includes campaigns started in Instantly: "+count(im.LeadsContacted)+
			" leads emailed, "+count(im.Replied)+" replies and "+count(im.Interested)+" opportunities, from "+
			"Instantly's own figures and counted on the day they happened.")
	}
	out.Notes = append(out.Notes,
		"Verification is priced at the verifier's current rate.",
		"Not included: staff time and anything paid for outside Karvon.")
	return out
}

// step builds one funnel step. format takes the cost and the count, already
// formatted, and gets " = <per unit>" appended when the count is not zero.
func step(key, label string, n, cents int64, format string) LeadCostStep {
	s := LeadCostStep{Key: key, Label: label, Count: n, CostCents: cents}
	s.Formula = fmt.Sprintf(format, usd(cents), count(n))
	if n > 0 {
		per := math.Round(float64(cents)/float64(n)*100) / 100
		s.CostPerUnitCents = &per
		s.Formula += " = " + usdUnit(per) + " each"
	} else {
		s.Formula += " — none yet, so no cost per unit"
	}
	return s
}

// usd formats cents as "$1,234.56".
func usd(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%s.%02d", sign, count(cents/100), cents%100)
}

// usdUnit formats a unit price in cents. Under a dollar it keeps four decimal places
// of a dollar so a fraction of a cent does not print as $0.00.
func usdUnit(cents float64) string {
	if cents >= 100 {
		return usd(int64(math.Round(cents)))
	}
	return "$" + strconv.FormatFloat(cents/100, 'f', 4, 64)
}

// count formats an integer with thousands separators.
func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + count(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func days(d float64) string {
	return strconv.FormatFloat(math.Round(d*10)/10, 'f', 1, 64)
}
