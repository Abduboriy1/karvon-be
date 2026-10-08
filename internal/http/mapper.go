package httpapi

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/stats"
	"github.com/bory/karvon-be/internal/verify"
)

// toAPIJob renders a job row the way the spec defines it, decoding the JSONB columns
// so the client never sees raw database documents.
func toAPIJob(row db.JobRow) (gen.Job, error) {
	cfg, err := scraper.DecodeConfig(row.Config)
	if err != nil {
		return gen.Job{}, err
	}
	st, err := scraper.DecodeStats(row.Stats)
	if err != nil {
		return gen.Job{}, err
	}

	sourceName := row.SourceName
	sourceKind := gen.SourceKind(row.SourceKind)
	return gen.Job{
		Id:         row.ID,
		Name:       row.Name,
		Status:     gen.JobStatus(row.Status),
		SourceId:   row.SourceID,
		SourceName: &sourceName,
		SourceKind: &sourceKind,
		Config:     toAPIJobConfig(cfg),
		Stats:      toAPIJobStats(st),
		Error:      row.Error,
		CreatedAt:  utc(row.CreatedAt),
		StartedAt:  utcPtr(row.StartedAt),
		FinishedAt: utcPtr(row.FinishedAt),
	}, nil
}

func toAPIJobConfig(cfg scraper.Config) gen.JobConfig {
	// Both halves are always emitted, empty string included, so clients can rely on
	// the keys being present.
	locations := make([]gen.Location, 0, len(cfg.Locations))
	for _, loc := range cfg.Locations {
		city, state := loc.City, loc.State
		locations = append(locations, gen.Location{City: &city, State: &state})
	}
	terms := cfg.Terms
	if terms == nil {
		terms = []string{}
	}
	maxPerQuery, concurrency, crawlEmails := cfg.MaxPerQuery, cfg.Concurrency, cfg.CrawlEmails
	out := gen.JobConfig{
		Terms:       terms,
		Locations:   &locations,
		MaxPerQuery: &maxPerQuery,
		Concurrency: &concurrency,
		CrawlEmails: &crawlEmails,
		RecrawlOf:   cfg.RecrawlOf,
	}
	if len(cfg.RecrawlTargets) > 0 {
		targets := make([]gen.RecrawlTarget, 0, len(cfg.RecrawlTargets))
		for _, target := range cfg.RecrawlTargets {
			targets = append(targets, gen.RecrawlTarget(target))
		}
		out.RecrawlTargets = &targets
	}
	if cfg.IsSocialScrape() {
		networks := make([]gen.SocialScrapeNetwork, 0, len(cfg.SocialNetworks))
		for _, network := range cfg.SocialNetworks {
			networks = append(networks, gen.SocialScrapeNetwork(network))
		}
		missingEmailOnly := cfg.SocialMissingEmailOnly
		out.SocialNetworks = &networks
		out.SocialMissingEmailOnly = &missingEmailOnly
	}
	return out
}

func toAPIJobStats(st scraper.Stats) gen.JobStats {
	queriesFailed := st.QueriesFailed
	duplicates := st.Duplicates
	return gen.JobStats{
		Duplicates:    &duplicates,
		QueriesTotal:  st.QueriesTotal,
		QueriesDone:   st.QueriesDone,
		QueriesFailed: &queriesFailed,
		ListingsFound: st.ListingsFound,
		SitesTotal:    st.SitesTotal,
		SitesCrawled:  st.SitesCrawled,
		EmailsFound:   st.EmailsFound,
		CostCents:     st.CostCents,
	}
}

func toAPIBusiness(row db.BusinessRow, socials []dbgen.BusinessSocial) gen.Business {
	emailsCount := int(row.EmailsCount)
	out := gen.Business{
		FirstJobName:               row.FirstJobName,
		PrimaryEmailVerifiedStatus: toAPITag(row.PrimaryEmailTag),
		Id:                         row.ID,
		PlaceId:                    row.PlaceID,
		Name:                       row.Name,
		Category:                   row.Category,
		Address:                    row.Address,
		City:                       row.City,
		State:                      row.State,
		Zip:                        row.Zip,
		Phone:                      row.Phone,
		Website:                    row.Website,
		Domain:                     row.Domain,
		Rating:                     row.Rating,
		Lat:                        row.Lat,
		Lng:                        row.Lng,
		Suppressed:                 row.Suppressed,
		Exclusion:                  toAPIExclusionRef(row.Exclusion),
		Notes:                      row.Notes,
		FirstJobId:                 row.FirstJobID,
		PrimaryEmail:               row.PrimaryEmail,
		EmailsCount:                &emailsCount,
		Socials:                    toAPISocials(socials),
		CreatedAt:                  utc(row.CreatedAt),
		UpdatedAt:                  utc(row.UpdatedAt),
	}
	if row.Reviews != nil {
		reviews := int(*row.Reviews)
		out.Reviews = &reviews
	}
	if row.PrimaryEmailSource != nil {
		source := gen.EmailSource(*row.PrimaryEmailSource)
		out.PrimaryEmailSource = &source
	}
	if row.PrimaryEmailScore != nil {
		score := int(*row.PrimaryEmailScore)
		out.PrimaryEmailVerificationScore = &score
	}
	return out
}

func toAPIBusinessDetail(detail business.Detail) gen.BusinessDetail {
	row := detail.Business
	emailsCount := int(row.EmailsCount)

	out := gen.BusinessDetail{
		Id:          row.ID,
		PlaceId:     row.PlaceID,
		Name:        row.Name,
		Category:    row.Category,
		Address:     row.Address,
		City:        row.City,
		State:       row.State,
		Zip:         row.Zip,
		Phone:       row.Phone,
		Website:     row.Website,
		Domain:      row.Domain,
		Rating:      row.Rating,
		Lat:         row.Lat,
		Lng:         row.Lng,
		Suppressed:  row.Suppressed,
		Exclusion:   toAPIExclusionRef(detail.Exclusion),
		Notes:       row.Notes,
		EmailsCount: &emailsCount,
		CreatedAt:   utc(row.CreatedAt),
		UpdatedAt:   utc(row.UpdatedAt),
	}
	if row.FirstJobName != "" {
		name := row.FirstJobName
		out.FirstJobName = &name
	}
	if row.Reviews != nil {
		reviews := int(*row.Reviews)
		out.Reviews = &reviews
	}
	if row.FirstJobID.Valid {
		id := row.FirstJobID.UUID
		out.FirstJobId = &id
	}
	if row.PrimaryEmail != "" {
		primary := row.PrimaryEmail
		out.PrimaryEmail = &primary
	}
	if row.PrimaryEmailSource != "" {
		source := gen.EmailSource(row.PrimaryEmailSource)
		out.PrimaryEmailSource = &source
	}
	if len(row.Raw) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(row.Raw, &raw); err == nil {
			out.Raw = &raw
		}
	}

	emails := make([]gen.BusinessEmail, 0, len(detail.Emails))
	for _, email := range detail.Emails {
		mapped := toAPIEmail(email)
		if ref, ok := detail.EmailExclusions[strings.ToLower(email.Email)]; ok {
			mapped.Exclusion = toAPIExclusionRef(&ref)
		}
		emails = append(emails, mapped)
	}
	out.Emails = &emails

	out.Socials = toAPISocials(detail.Socials)
	return out
}

func toAPISocials(rows []dbgen.BusinessSocial) *[]gen.BusinessSocial {
	out := make([]gen.BusinessSocial, 0, len(rows))
	for _, social := range rows {
		out = append(out, gen.BusinessSocial{
			Id:      social.ID,
			Network: gen.SocialNetwork(social.Network),
			Handle:  social.Handle,
			Url:     social.Url,
			PageUrl: social.PageUrl,
			FoundAt: utc(social.FoundAt),
		})
	}
	return &out
}

func toAPIEmail(row dbgen.ListBusinessEmailsWithVerificationRow) gen.BusinessEmail {
	out := gen.BusinessEmail{
		Id:             row.ID,
		Email:          row.Email,
		Source:         gen.EmailSource(row.Source),
		PageUrl:        row.PageUrl,
		IsPrimary:      row.IsPrimary,
		VerifiedStatus: toAPITag(row.VerificationTag),
		TypoSuggestion: row.TypoSuggestion,
		FoundAt:        utc(row.FoundAt),
	}
	if row.VerificationID.Valid {
		id := row.VerificationID.UUID
		out.VerificationId = &id
	}
	if row.VerificationScore != nil {
		score := int(*row.VerificationScore)
		out.VerificationScore = &score
	}
	return out
}

func toAPISource(row dbgen.Source) gen.Source {
	return gen.Source{
		Id:             row.ID,
		Kind:           gen.SourceKind(row.Kind),
		Role:           gen.SourceRole(row.Role),
		Name:           row.Name,
		HasKey:         len(row.ApiKeyEnc) > 0,
		CostPer1kCents: int(row.CostPer1kCents),
		Enabled:        row.Enabled,
		MaxActiveRuns:  int(row.MaxActiveRuns),
		LastTestedAt:   utcPtr(row.LastTestedAt),
		LastTestOk:     row.LastTestOk,
		CreatedAt:      utc(row.CreatedAt),
		UpdatedAt:      utc(row.UpdatedAt),
	}
}

func toAPIStats(in stats.Scraper) (gen.ScraperStats, error) {
	perJob := make([]gen.ScraperStatsJobEmails, 0, len(in.EmailsPerJob))
	for _, row := range in.EmailsPerJob {
		perJob = append(perJob, gen.ScraperStatsJobEmails{
			JobId:  row.JobID,
			Name:   row.Name,
			Emails: int(row.Emails),
		})
	}

	emailsTotal := in.EmailsTotal
	out := gen.ScraperStats{
		Businesses:   in.Businesses,
		WithEmail:    in.WithEmail,
		EmailsTotal:  &emailsTotal,
		JobsTotal:    in.JobsTotal,
		EmailsPerJob: perJob,
	}
	if in.LastJob != nil {
		job, err := toAPIJob(*in.LastJob)
		if err != nil {
			return gen.ScraperStats{}, err
		}
		out.LastJob = &job
	}
	return out, nil
}

func pageMeta(page, perPage int, total int64) gen.PageMeta {
	return gen.PageMeta{Page: page, PerPage: perPage, Total: total}
}

// utc normalises a timestamp for the API. The shared convention with the frontend is
// RFC 3339 in UTC, and Postgres hands pgx timestamps in the process's local zone.
func utc(t time.Time) time.Time { return t.UTC() }

// utcPtr is utc for nullable columns.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	value := t.UTC()
	return &value
}

/* --------------------------------------------------------------- verification */

func toAPIVerification(row db.VerificationRow) gen.EmailVerification {
	tag := gen.VerificationTag(row.VerificationTag)
	out := gen.EmailVerification{
		Id:              row.ID,
		Email:           row.Email,
		Domain:          row.Domain,
		Pass1Score:      int(row.Pass1Score),
		Pass1HardFail:   row.Pass1HardFail,
		Pass1VerifiedAt: utcPtr(row.Pass1VerifiedAt),
		FreeScore:       int(row.FreeScore),
		FreeScoredAt:    utcPtr(row.FreeScoredAt),
		Pass2VerifiedAt: utcPtr(row.Pass2VerifiedAt),
		// Non-null here is what the UI reads to say "this one is spent".
		ThirdPartySentAt: utcPtr(row.ThirdPartySentAt),
		Pass2Credits:     int(row.Pass2Credits),
		FinalScore:       int(row.FinalScore),
		VerificationTag:  tag,
		TagLabel:         verify.Tag(row.VerificationTag).Label(),
		TypoSuggestion:   row.TypoSuggestion,
		LastError:        row.LastError,
		BusinessCount:    int(row.BusinessCount),
		Exclusion:        toAPIExclusionRef(row.Exclusion),
		UpdatedAt:        utc(row.UpdatedAt),
	}
	if row.Pass2Score != nil {
		score := int(*row.Pass2Score)
		out.Pass2Score = &score
	}
	out.Pass2Status = toAPIPass2Status(row.Pass2Status)
	return out
}

func toAPIVerificationDetail(detail verify.Detail) gen.EmailVerificationDetail {
	row := detail.Row
	tag := gen.VerificationTag(row.VerificationTag)

	out := gen.EmailVerificationDetail{
		Id:              row.ID,
		Email:           row.Email,
		Domain:          row.Domain,
		Pass1Score:      int(row.Pass1Score),
		Pass1HardFail:   row.Pass1HardFail,
		Pass1VerifiedAt: utcPtr(row.Pass1VerifiedAt),
		FreeScore:       int(row.FreeScore),
		FreeScoredAt:    utcPtr(row.FreeScoredAt),
		Pass2VerifiedAt: utcPtr(row.Pass2VerifiedAt),
		// Non-null here is what the UI reads to say "this one is spent".
		ThirdPartySentAt: utcPtr(row.ThirdPartySentAt),
		Pass2Credits:     int(row.Pass2Credits),
		FinalScore:       int(row.FinalScore),
		VerificationTag:  tag,
		TagLabel:         verify.Tag(row.VerificationTag).Label(),
		TypoSuggestion:   row.TypoSuggestion,
		LastError:        row.LastError,
		BusinessCount:    len(detail.Businesses),
		Exclusion:        toAPIExclusionRef(detail.Exclusion),
		UpdatedAt:        utc(row.UpdatedAt),
	}
	if row.Pass2Score != nil {
		score := int(*row.Pass2Score)
		out.Pass2Score = &score
	}
	out.Pass2Status = toAPIPass2Status(row.Pass2Status)

	checks := make([]gen.VerificationCheck, 0, len(detail.Checks))
	for _, check := range detail.Checks {
		item := gen.VerificationCheck{
			Key:    check.Key,
			Label:  check.Label,
			Status: gen.VerificationCheckStatus(check.Status),
			Points: check.Points,
			Max:    check.Max,
		}
		if check.Detail != "" {
			item.Detail = &check.Detail
		}
		checks = append(checks, item)
	}
	out.Pass1Checks = &checks

	businesses := make([]gen.VerificationBusiness, 0, len(detail.Businesses))
	for _, biz := range detail.Businesses {
		businesses = append(businesses, gen.VerificationBusiness{Id: biz.ID, Name: biz.Name})
	}
	out.Businesses = &businesses

	providers := toAPIProviderResults(detail.Providers)
	out.ProviderResults = &providers

	if len(row.Pass2Raw) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(row.Pass2Raw, &raw); err == nil {
			out.Pass2Raw = &raw
		}
	}
	return out
}

// toAPIProviderResults renders the weighted breakdown. Every weighted provider
// always appears, including ones that contributed nothing, so a client can explain a
// score in full without knowing the provider list itself.
func toAPIProviderResults(scores []verify.ProviderScore) []gen.VerificationProviderResult {
	out := make([]gen.VerificationProviderResult, 0, len(scores))
	for _, score := range scores {
		item := gen.VerificationProviderResult{
			Provider:        score.Provider,
			Label:           score.Label,
			Status:          gen.VerificationProviderStatus(score.Status),
			Score:           score.Score,
			Weight:          score.Weight,
			EffectiveWeight: score.EffectiveWeight,
			Contribution:    score.Contribution,
		}
		if score.Reason != "" {
			item.Reason = &score.Reason
		}
		if score.Disqualifying {
			disqualifying := true
			item.Disqualifying = &disqualifying
		}
		if score.Error != "" {
			item.Error = &score.Error
		}
		if len(score.Metadata) > 0 {
			meta := score.Metadata
			item.Metadata = &meta
		}
		if score.DurationMs > 0 {
			ms := score.DurationMs
			item.DurationMs = &ms
		}
		out = append(out, item)
	}
	return out
}

// toAPIVerificationSettings renders the settings view, including the provider
// catalogue the settings page draws its controls from.
func toAPIVerificationSettings(view verify.SettingsView) gen.VerificationSettingsView {
	providers := make([]gen.VerificationProviderHealth, 0, len(view.Providers))
	for _, p := range view.Providers {
		item := gen.VerificationProviderHealth{
			Provider:    p.Provider,
			Label:       p.Label,
			Stage:       gen.VerificationProviderHealthStage(p.Stage),
			Weighted:    p.Weighted,
			Toggleable:  p.Toggleable,
			Description: p.Description,
			Enabled:     p.Enabled,
			Weight:      p.Weight,
			Healthy:     p.Healthy,
		}
		if p.Error != "" {
			item.Error = &p.Error
		}
		providers = append(providers, item)
	}

	return gen.VerificationSettingsView{
		Settings:     toAPISettings(view.Settings),
		Providers:    providers,
		FreeMaxScore: view.FreeMaxScore,
		WeightTotal:  view.WeightTotal,
		UpdatedAt:    utcPtr(view.UpdatedAt),
	}
}

func toAPISettings(settings verify.Settings) gen.VerificationSettings {
	weights := make(map[string]int, len(settings.Weights))
	for key, weight := range settings.Weights {
		weights[key] = weight
	}
	enabled := make(map[string]bool, len(settings.Enabled))
	for key, on := range settings.Enabled {
		enabled[key] = on
	}
	return gen.VerificationSettings{
		Weights:        weights,
		Enabled:        enabled,
		PaidEnabled:    settings.PaidEnabled,
		PaidThreshold:  settings.PaidThreshold,
		PaidMinScore:   settings.PaidMinScore,
		AutoSelfVerify: &settings.AutoSelfVerify,
	}
}

// toDomainSettings maps a submitted settings body onto the domain type. Validation
// happens in the domain, so unknown keys and impossible totals produce the same
// field errors whatever route they arrive by.
//
// auto_self_verify is optional on the wire so a client written before it existed
// keeps working; leaving it out keeps the current value.
func toDomainSettings(in gen.VerificationSettingsUpdate, current verify.Settings) verify.Settings {
	out := verify.Settings{
		Weights:        make(map[verify.Key]int, len(in.Weights)),
		Enabled:        make(map[verify.Key]bool, len(in.Enabled)),
		PaidEnabled:    in.PaidEnabled,
		PaidThreshold:  in.PaidThreshold,
		PaidMinScore:   in.PaidMinScore,
		AutoSelfVerify: current.AutoSelfVerify,
	}
	if in.AutoSelfVerify != nil {
		out.AutoSelfVerify = *in.AutoSelfVerify
	}
	for key, weight := range in.Weights {
		out.Weights[key] = weight
	}
	for key, on := range in.Enabled {
		out.Enabled[key] = on
	}
	return out
}

func toAPIPass2Status(value *string) *gen.Pass2Status {
	if value == nil || *value == "" {
		return nil
	}
	status := gen.Pass2Status(*value)
	return &status
}

// toAPITag maps a nullable stored tag onto the wire enum.
func toAPITag(value *string) *gen.VerificationTag {
	if value == nil || *value == "" {
		return nil
	}
	tag := gen.VerificationTag(*value)
	return &tag
}

func toAPIVerificationRun(row dbgen.VerificationRun) gen.VerificationRun {
	out := gen.VerificationRun{
		Id:           row.ID,
		Pass:         gen.VerificationPass(row.Pass),
		Status:       gen.JobStatus(row.Status),
		Filter:       toAPIRunFilter(row.Filter),
		Total:        int(row.Total),
		Done:         int(row.Done),
		Failed:       int(row.Failed),
		Skipped:      int(row.Skipped),
		CreditsUsed:  int(row.CreditsUsed),
		EstCostCents: row.EstCostCents,
		Error:        row.Error,
		CreatedAt:    utc(row.CreatedAt),
		StartedAt:    utcPtr(row.StartedAt),
		FinishedAt:   utcPtr(row.FinishedAt),
		Auto:         row.Auto,
	}
	if row.SourceID.Valid {
		id := row.SourceID.UUID
		out.SourceId = &id
	}
	return out
}

// toAPIRunFilter echoes back the filter the run was created with. A stored document
// that cannot be decoded is reported as an empty filter rather than failing the
// request: the run's counters still matter.
func toAPIRunFilter(raw []byte) gen.VerificationRunFilter {
	out := gen.VerificationRunFilter{}
	if len(raw) == 0 {
		return out
	}
	var filter verify.RunFilter
	if err := json.Unmarshal(raw, &filter); err != nil {
		return out
	}

	scope := gen.VerificationRunFilterScope(filter.Scope)
	out.Scope = &scope
	if len(filter.IDs) > 0 {
		ids := filter.IDs
		out.Ids = &ids
	}
	if len(filter.BusinessIDs) > 0 {
		ids := filter.BusinessIDs
		out.BusinessIds = &ids
	}
	out.JobId = filter.JobID
	if len(filter.Tags) > 0 {
		tags := make([]gen.VerificationTag, 0, len(filter.Tags))
		for _, tag := range filter.Tags {
			tags = append(tags, gen.VerificationTag(tag))
		}
		out.Tags = &tags
	}
	out.MinScore = filter.MinScore
	if filter.IncludeSuppressed {
		include := true
		out.IncludeSuppressed = &include
	}
	out.StaleAfterDays = filter.StaleAfterDays
	return out
}

func toAPIRunEstimate(in verify.Estimate) gen.VerificationRunEstimate {
	out := gen.VerificationRunEstimate{
		Emails:         in.Emails,
		NeedsSelf:      in.NeedsSelf,
		Cached:         in.Cached,
		CreditsNeeded:  in.CreditsNeeded,
		EstCostCents:   in.EstCostCents,
		BalanceCredits: in.BalanceCredits,
	}
	if in.CostPer1kCents > 0 {
		cost := in.CostPer1kCents
		out.CostPer1kCents = &cost
	}
	return out
}

func toAPIVerificationStats(in verify.Stats) gen.VerificationStats {
	return gen.VerificationStats{
		Total: in.Total,
		ByTag: gen.VerificationTagCounts{
			Green:      in.ByTag[verify.TagGreen],
			LightGreen: in.ByTag[verify.TagLightGreen],
			Yellow:     in.ByTag[verify.TagYellow],
			Orange:     in.ByTag[verify.TagOrange],
			Red:        in.ByTag[verify.TagRed],
		},
		SelfVerified:            in.SelfVerified,
		ThirdPartyVerified:      in.ThirdPartyVerified,
		NeedsSelf:               in.NeedsSelf,
		QualifyingForThirdParty: in.QualifyingForThird,
		QualifyingEstCostCents:  in.QualifyingEstCostCents,
		CreditsUsedTotal:        in.CreditsUsedTotal,
		CreditsUsed30d:          in.CreditsUsed30d,
		BalanceCredits:          in.BalanceCredits,
		ActiveRuns:              in.ActiveRuns,
		LastSelfRunAt:           utcPtr(in.LastSelfRunAt),
		LastThirdPartyRunAt:     utcPtr(in.LastThirdPartyRunAt),
	}
}
