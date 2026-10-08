package stats

import (
	"context"
	"math"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
)

// Report limits and defaults.
const (
	// DefaultReportDays is the window when the client names no dates.
	DefaultReportDays = 30
	// MaxReportDays caps a window at two years.
	MaxReportDays = 731
	// MaxReportBuckets keeps the series small enough to draw and to print.
	MaxReportBuckets = 400
	// ReportListLimit is how many rows each breakdown and the campaign table carry.
	ReportListLimit = 10
)

// Bucket sizes the series can be cut into.
const (
	BucketDay   = "day"
	BucketWeek  = "week"
	BucketMonth = "month"
)

// ReportInput is the request behind GET /dashboard/report. From and To are calendar
// dates in TZ, both inclusive; zero values take the defaults.
type ReportInput struct {
	From   time.Time
	To     time.Time
	TZ     string
	Bucket string
}

// ReportRange is the window a report covers, resolved.
type ReportRange struct {
	From     time.Time // first day, inclusive
	To       time.Time // last day, inclusive
	Days     int
	TZ       string
	Bucket   string
	Start    time.Time // instant the window opens
	End      time.Time // instant the window closes (exclusive)
	PrevFrom time.Time
	PrevTo   time.Time
}

// Summary is one window's figures, priced, with the ratios a reader asks for next.
type Summary struct {
	db.DashboardSummary
	Costs             Costs
	LeadCost          LeadCost
	EmailFindRate     float64
	OpenRate          float64
	ReplyRate         float64
	PositiveReplyRate float64
	BounceRate        float64
}

// Point is one bucket of the series, priced the same way as a Summary.
type Point struct {
	db.DashboardPoint
	MailboxesCents int64
	ToolsCents     int64
	CostCents      int64
}

// Report is the whole client dashboard.
type Report struct {
	Range       ReportRange
	GeneratedAt time.Time
	Current     Summary
	Previous    Summary
	AllTime     Summary
	Series      []Point
	Campaigns   []db.DashboardCampaign
	Breakdowns  db.DashboardBreakdowns
}

// Report builds the cross-module client dashboard for one window, the window before
// it (for the deltas) and all time.
func (s *Service) Report(ctx context.Context, in ReportInput) (Report, error) {
	rng, err := ResolveRange(in, time.Now())
	if err != nil {
		return Report{}, err
	}

	out := Report{Range: rng, GeneratedAt: time.Now().UTC()}

	cur, err := s.store.DashboardSummary(ctx, rng.Start, rng.End)
	if err != nil {
		return Report{}, apperr.Internal(err)
	}
	prev, err := s.store.DashboardSummary(ctx, rng.Start.AddDate(0, 0, -rng.Days), rng.Start)
	if err != nil {
		return Report{}, apperr.Internal(err)
	}
	all, err := s.store.DashboardSummary(ctx, time.Unix(0, 0).UTC(), rng.End)
	if err != nil {
		return Report{}, apperr.Internal(err)
	}
	out.Current = s.summarise(cur, false)
	out.Previous = s.summarise(prev, false)
	out.AllTime = s.summarise(all, true)

	series, err := s.store.DashboardSeries(ctx, rng.Start, rng.End, rng.Bucket, rng.TZ)
	if err != nil {
		return Report{}, apperr.Internal(err)
	}
	out.Series = make([]Point, 0, len(series))
	for _, p := range series {
		pt := Point{
			DashboardPoint: p,
			MailboxesCents: s.recurring.mailboxCents(p.MailboxDays),
			ToolsCents:     s.recurring.toolsCents(p.ActiveDays),
		}
		pt.CostCents = p.ScrapingCents + p.VerificationCents + p.DomainsCents + pt.MailboxesCents + pt.ToolsCents
		out.Series = append(out.Series, pt)
	}
	if out.Campaigns, err = s.store.DashboardCampaigns(ctx, rng.Start, rng.End, ReportListLimit); err != nil {
		return Report{}, apperr.Internal(err)
	}
	if out.Breakdowns, err = s.store.DashboardBreakdowns(ctx, rng.Start, rng.End, ReportListLimit); err != nil {
		return Report{}, apperr.Internal(err)
	}
	return out, nil
}

// ResolveRange applies the defaults and the limits to a request. now is passed in so
// the defaults can be tested.
func ResolveRange(in ReportInput, now time.Time) (ReportRange, error) {
	tz := in.TZ
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return ReportRange{}, apperr.Validation("unknown time zone",
			apperr.FieldError{Field: "tz", Message: "must be an IANA time zone such as Europe/London"})
	}

	to := civil(in.To, loc)
	if in.To.IsZero() {
		to = civil(now.In(loc), loc)
	}
	from := civil(in.From, loc)
	if in.From.IsZero() {
		from = to.AddDate(0, 0, -(DefaultReportDays - 1))
	}
	if from.After(to) {
		return ReportRange{}, apperr.Validation("from is after to",
			apperr.FieldError{Field: "from", Message: "must be on or before to"})
	}

	// Calendar days, counted on dates so a DST change does not shorten one.
	days := int(math.Round(dateOnly(to).Sub(dateOnly(from)).Hours()/24)) + 1
	if days > MaxReportDays {
		return ReportRange{}, apperr.Validation("window too long",
			apperr.FieldError{Field: "from", Message: "the window may span at most 731 days"})
	}

	bucket := in.Bucket
	switch bucket {
	case "":
		bucket = autoBucket(days)
	case BucketDay, BucketWeek, BucketMonth:
	default:
		return ReportRange{}, apperr.Validation("unknown bucket",
			apperr.FieldError{Field: "bucket", Message: "must be day, week or month"})
	}
	if bucket == BucketDay && days > MaxReportBuckets {
		return ReportRange{}, apperr.Validation("too many buckets",
			apperr.FieldError{Field: "bucket", Message: "use week or month for a window over 400 days"})
	}

	return ReportRange{
		From:     from,
		To:       to,
		Days:     days,
		TZ:       tz,
		Bucket:   bucket,
		Start:    from,
		End:      to.AddDate(0, 0, 1),
		PrevFrom: from.AddDate(0, 0, -days),
		PrevTo:   from.AddDate(0, 0, -1),
	}, nil
}

// autoBucket keeps a series between roughly 7 and 60 points.
func autoBucket(days int) string {
	switch {
	case days <= 62:
		return BucketDay
	case days <= 182:
		return BucketWeek
	default:
		return BucketMonth
	}
}

// civil is midnight of t's calendar date in loc.
func civil(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Service) summarise(in db.DashboardSummary, allTime bool) Summary {
	costs := s.recurring.price(in)
	return Summary{
		DashboardSummary:  in,
		Costs:             costs,
		LeadCost:          s.recurring.leadCost(in, costs, allTime),
		EmailFindRate:     ratio(in.BusinessesWithEmail, in.Businesses),
		OpenRate:          ratio(in.Opened, in.Sends),
		ReplyRate:         ratio(in.Replied, in.Sends),
		PositiveReplyRate: ratio(in.PositiveReplies, in.Sends),
		BounceRate:        ratio(in.Bounced, in.Sends),
	}
}

// ratio is a/b to four decimal places, 0 when b is 0.
func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10000) / 10000
}
