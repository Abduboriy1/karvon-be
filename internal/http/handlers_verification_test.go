package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/verify"
)

func sampleVerificationRow() db.VerificationRow {
	hardFail := verify.CheckRoleAccount
	pass2 := "deliverable"
	score := int32(100)
	verifiedAt := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	suggestion := "owner@gmail.com"

	return db.VerificationRow{
		ID:              testVerifyID,
		Email:           "jane.doe@ironworksgym.com",
		Domain:          "ironworksgym.com",
		Pass1Score:      85,
		Pass1HardFail:   &hardFail,
		Pass1VerifiedAt: &verifiedAt,
		Pass2Score:      &score,
		Pass2Status:     &pass2,
		Pass2VerifiedAt: &verifiedAt,
		Pass2Credits:    1,
		FinalScore:      100,
		VerificationTag: string(verify.TagGreen),
		TypoSuggestion:  &suggestion,
		BusinessCount:   2,
		UpdatedAt:       verifiedAt,
	}
}

// The tag never travels without the text that goes with it, because the UI is not
// allowed to convey a result by colour alone.
func TestListVerificationsReturnsTagsWithLabels(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.list = verify.ListResult{
		Rows:  []db.VerificationRow{sampleVerificationRow()},
		Total: 137,
	}

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/emails?per_page=25&page=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Data []struct {
			Email           string `json:"email"`
			Pass1Score      int    `json:"pass1_score"`
			Pass2Score      *int   `json:"pass2_score"`
			FinalScore      int    `json:"final_score"`
			VerificationTag string `json:"verification_tag"`
			TagLabel        string `json:"tag_label"`
			TypoSuggestion  string `json:"typo_suggestion"`
			BusinessCount   int    `json:"business_count"`
		} `json:"data"`
		Meta struct {
			Page    int   `json:"page"`
			PerPage int   `json:"per_page"`
			Total   int64 `json:"total"`
		} `json:"meta"`
	}](t, rec)

	if len(payload.Data) != 1 {
		t.Fatalf("data = %+v", payload.Data)
	}
	row := payload.Data[0]
	if row.VerificationTag != string(verify.TagGreen) || row.TagLabel != "Verified" {
		t.Errorf("tag = %q / %q, want green / Verified", row.VerificationTag, row.TagLabel)
	}
	if row.Pass1Score != 85 || row.Pass2Score == nil || *row.Pass2Score != 100 || row.FinalScore != 100 {
		t.Errorf("scores = %+v, want both passes reported separately", row)
	}
	if row.TypoSuggestion != "owner@gmail.com" || row.BusinessCount != 2 {
		t.Errorf("row = %+v", row)
	}
	if payload.Meta.Total != 137 || payload.Meta.Page != 2 || payload.Meta.PerPage != 25 {
		t.Errorf("meta = %+v", payload.Meta)
	}
}

func TestListVerificationsForwardsEveryFilter(t *testing.T) {
	handler, deps := newTestServer(t)

	target := "/api/v1/verification/emails?tag=red&tag=orange&min_score=10&max_score=80" +
		"&pass2_status=risky&has_typo=true&q=iron&business_id=" + testBizID.String() +
		"&job_id=" + testJobID.String() + "&include_suppressed=true&sort=email:asc"

	if rec := do(t, handler, http.MethodGet, target, ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := deps.verification.lastFilter
	if len(filter.Tags) != 2 || filter.Tags[0] != "red" || filter.Tags[1] != "orange" {
		t.Errorf("tags = %v", filter.Tags)
	}
	if filter.MinScore == nil || *filter.MinScore != 10 {
		t.Errorf("min_score = %v", filter.MinScore)
	}
	if filter.MaxScore == nil || *filter.MaxScore != 80 {
		t.Errorf("max_score = %v", filter.MaxScore)
	}
	if filter.Pass2Status == nil || *filter.Pass2Status != "risky" {
		t.Errorf("pass2_status = %v", filter.Pass2Status)
	}
	if filter.HasTypo == nil || !*filter.HasTypo {
		t.Errorf("has_typo = %v", filter.HasTypo)
	}
	if filter.Q == nil || *filter.Q != "iron" {
		t.Errorf("q = %v", filter.Q)
	}
	if len(filter.BusinessIDs) != 1 || filter.BusinessIDs[0] != testBizID {
		t.Errorf("business_ids = %v", filter.BusinessIDs)
	}
	if filter.JobID == nil || *filter.JobID != testJobID {
		t.Errorf("job_id = %v", filter.JobID)
	}
	if !filter.IncludeSuppressed {
		t.Error("include_suppressed was not forwarded")
	}
	if deps.verification.lastSort != "email:asc" {
		t.Errorf("sort = %q", deps.verification.lastSort)
	}
}

// "Needs third party" has to mean exactly what a paid run would pick up, or the
// count beside the button would not match what the button does.
func TestNeedsThirdPartyAppliesTheBandAndTheCache(t *testing.T) {
	handler, deps := newTestServer(t)

	if rec := do(t, handler, http.MethodGet,
		"/api/v1/verification/emails?needs_third_party=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := deps.verification.lastFilter
	if filter.FreeComplete == nil || !*filter.FreeComplete {
		t.Error("the gate does not require a completed free stage")
	}
	defaults := verify.DefaultSettings()
	if filter.MinFreeScore == nil || *filter.MinFreeScore != defaults.PaidMinScore {
		t.Errorf("min free score = %v, want the configured floor %d",
			deref(filter.MinFreeScore), defaults.PaidMinScore)
	}
	// The ceiling is forwarded whatever it is set to, so the count beside the button
	// always matches the band a paid run would actually pick up.
	if filter.MaxFreeScore == nil || *filter.MaxFreeScore != defaults.PaidThreshold {
		t.Errorf("max free score = %v, want the configured threshold %d",
			deref(filter.MaxFreeScore), defaults.PaidThreshold)
	}
	if filter.ThirdPartySent == nil || *filter.ThirdPartySent {
		t.Error("the send lock is not applied, so addresses already sent to a third party would be counted")
	}
}

func TestListVerificationsRejectsAnUnknownSort(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/emails?sort=nonsense", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if field := decodeError(t, rec).Fields["sort"]; field == "" {
		t.Error("the error does not name the sort parameter")
	}
}

func TestGetVerificationReturnsTheBreakdown(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.detail = verify.Detail{
		Row: dbgen.EmailVerification{
			ID:              testVerifyID,
			Email:           "jane.doe@ironworksgym.com",
			Domain:          "ironworksgym.com",
			Pass1Score:      85,
			FinalScore:      85,
			VerificationTag: string(verify.TagLightGreen),
			Pass2Raw:        []byte(`{"state":"deliverable"}`),
			UpdatedAt:       time.Now().UTC(),
		},
		Checks: []verify.CheckResult{
			{Key: verify.CheckMX, Label: "MX records present", Status: verify.CheckPass, Points: 30, Max: 30},
			{Key: verify.CheckSPF, Label: "SPF record present", Status: verify.CheckFail, Max: 5, Detail: "no SPF record was published"},
		},
		Businesses: []dbgen.ListBusinessesForEmailRow{{ID: testBizID, Name: "Iron Works Gym"}},
	}

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/emails/"+testVerifyID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		TagLabel    string `json:"tag_label"`
		Pass1Checks []struct {
			Key    string `json:"key"`
			Label  string `json:"label"`
			Status string `json:"status"`
			Points int    `json:"points"`
			Max    int    `json:"max"`
			Detail string `json:"detail"`
		} `json:"pass1_checks"`
		Pass2Raw   map[string]any `json:"pass2_raw"`
		Businesses []struct {
			Name string `json:"name"`
		} `json:"businesses"`
	}](t, rec)

	if payload.TagLabel != "Likely valid" {
		t.Errorf("label = %q", payload.TagLabel)
	}
	if len(payload.Pass1Checks) != 2 {
		t.Fatalf("checks = %+v", payload.Pass1Checks)
	}
	if payload.Pass1Checks[0].Points != 30 || payload.Pass1Checks[1].Points != 0 {
		t.Errorf("points = %+v", payload.Pass1Checks)
	}
	if payload.Pass1Checks[1].Detail == "" {
		t.Error("a failed check must explain itself")
	}
	if payload.Pass2Raw["state"] != "deliverable" {
		t.Errorf("raw payload = %v", payload.Pass2Raw)
	}
	if len(payload.Businesses) != 1 || payload.Businesses[0].Name != "Iron Works Gym" {
		t.Errorf("businesses = %+v", payload.Businesses)
	}
}

func TestSingleAddressActionsQueueTheRightPass(t *testing.T) {
	tests := []struct {
		path string
		want verify.Pass
	}{
		{path: "/self", want: verify.PassSelf},
		{path: "/third-party", want: verify.PassThirdParty},
	}

	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			handler, deps := newTestServer(t)

			rec := do(t, handler, http.MethodPost,
				"/api/v1/verification/emails/"+testVerifyID.String()+tt.path, "")
			// The work is queued, not done.
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
			}
			if len(deps.verification.verifiedOne) != 1 || deps.verification.verifiedOne[0] != testVerifyID {
				t.Errorf("verified = %v", deps.verification.verifiedOne)
			}
			if deps.verification.verifiedPass != tt.want {
				t.Errorf("pass = %q, want %q", deps.verification.verifiedPass, tt.want)
			}
		})
	}
}

// Refusing a paid check the pipeline would skip is better than accepting it and
// silently doing nothing.
func TestThirdPartyActionSurfacesAConflict(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.err = apperr.Conflict("self verification scored 20, below the 50 needed for a paid check")

	rec := do(t, handler, http.MethodPost,
		"/api/v1/verification/emails/"+testVerifyID.String()+"/third-party", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if msg := decodeError(t, rec).Message; msg == "" {
		t.Error("the refusal carries no explanation")
	}
}

func TestEstimateRunReportsCostAndExclusions(t *testing.T) {
	handler, deps := newTestServer(t)
	balance := int64(4321)
	deps.verification.estimate = verify.Estimate{
		Emails: 120, NeedsSelf: 40, Cached: 15, CreditsNeeded: 120,
		CostPer1kCents: 500, EstCostCents: 60, BalanceCredits: &balance,
	}

	rec := do(t, handler, http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all","tags":["yellow"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Emails         int64  `json:"emails"`
		NeedsSelf      int64  `json:"needs_self"`
		Cached         int64  `json:"cached"`
		CreditsNeeded  int64  `json:"credits_needed"`
		CostPer1kCents int    `json:"cost_per_1k_cents"`
		EstCostCents   int64  `json:"est_cost_cents"`
		BalanceCredits *int64 `json:"balance_credits"`
	}](t, rec)

	if payload.Emails != 120 || payload.NeedsSelf != 40 || payload.Cached != 15 {
		t.Errorf("counts = %+v", payload)
	}
	if payload.EstCostCents != 60 || payload.CostPer1kCents != 500 {
		t.Errorf("cost = %+v", payload)
	}
	if payload.BalanceCredits == nil || *payload.BalanceCredits != 4321 {
		t.Errorf("balance = %v", payload.BalanceCredits)
	}

	if deps.verification.lastRunInput.Pass != verify.PassThirdParty {
		t.Errorf("pass = %q", deps.verification.lastRunInput.Pass)
	}
	filter := deps.verification.lastRunInput.Filter
	if filter.Scope != verify.ScopeAll || len(filter.Tags) != 1 || filter.Tags[0] != "yellow" {
		t.Errorf("filter = %+v", filter)
	}
}

func TestCreateRunForwardsTheApprovedCeiling(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/verification/runs",
		`{"pass":"third_party","filter":{"scope":"selection","ids":["`+testVerifyID.String()+`"]},"max_cost_cents":250}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	in := deps.verification.lastRunInput
	if in.MaxCostCents == nil || *in.MaxCostCents != 250 {
		t.Fatalf("max_cost_cents = %v, want the approved ceiling", in.MaxCostCents)
	}
	if in.Filter.Scope != verify.ScopeSelection || len(in.Filter.IDs) != 1 {
		t.Errorf("filter = %+v", in.Filter)
	}
}

// An unrecognised field is named rather than ignored, which is how a client finds
// out it is sending something the server will not act on.
func TestCreateRunRejectsAnUnknownField(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/verification/runs",
		`{"pass":"self","unexpected":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if field := decodeError(t, rec).Fields["unexpected"]; field == "" {
		t.Errorf("the error does not name the unknown field: %s", rec.Body.String())
	}
}

func TestCreateRunRejectsMalformedJSON(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/verification/runs", `{"pass":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRunEndpointsRoundTrip(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.runs = verify.RunListResult{
		Runs:  []dbgen.VerificationRun{sampleVerificationRun()},
		Total: 1,
	}

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/runs?pass=self&status=queued", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body.String())
	}
	list := decodeJSONBody[struct {
		Data []struct {
			Pass   string `json:"pass"`
			Status string `json:"status"`
			Total  int    `json:"total"`
			Filter struct {
				Scope string `json:"scope"`
			} `json:"filter"`
		} `json:"data"`
	}](t, rec)
	if len(list.Data) != 1 {
		t.Fatalf("runs = %+v", list.Data)
	}
	if list.Data[0].Pass != "self" || list.Data[0].Total != 3 {
		t.Errorf("run = %+v", list.Data[0])
	}
	// The filter is echoed back so the history page can explain what was submitted.
	if list.Data[0].Filter.Scope != "all" {
		t.Errorf("filter scope = %q", list.Data[0].Filter.Scope)
	}

	if rec := do(t, handler, http.MethodGet, "/api/v1/verification/runs/"+testRunID.String(), ""); rec.Code != http.StatusOK {
		t.Errorf("get status = %d", rec.Code)
	}

	rec = do(t, handler, http.MethodPost, "/api/v1/verification/runs/"+testRunID.String()+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Errorf("cancel status = %d", rec.Code)
	}
	if len(deps.verification.cancelled) != 1 || deps.verification.cancelled[0] != testRunID {
		t.Errorf("cancelled = %v", deps.verification.cancelled)
	}
}

func TestApplyTypoReturnsTheCorrectedAddress(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.detail = verify.Detail{
		Row: dbgen.EmailVerification{
			ID:              uuid.New(),
			Email:           "owner@gmail.com",
			Domain:          "gmail.com",
			VerificationTag: string(verify.TagYellow),
			UpdatedAt:       time.Now().UTC(),
		},
	}

	rec := do(t, handler, http.MethodPost,
		"/api/v1/verification/emails/"+testVerifyID.String()+"/apply-typo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(deps.verification.appliedTypoTo) != 1 {
		t.Fatalf("applied = %v", deps.verification.appliedTypoTo)
	}

	payload := decodeJSONBody[struct {
		Email    string `json:"email"`
		TagLabel string `json:"tag_label"`
	}](t, rec)
	if payload.Email != "owner@gmail.com" {
		t.Errorf("email = %q, want the corrected address", payload.Email)
	}
	if payload.TagLabel != "Uncertain" {
		t.Errorf("label = %q", payload.TagLabel)
	}
}

func TestVerificationStatsExposesTheTagDistribution(t *testing.T) {
	handler, deps := newTestServer(t)
	balance := int64(900)
	lastRun := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	deps.verification.stats = verify.Stats{
		Total: 10,
		ByTag: map[verify.Tag]int64{
			verify.TagGreen: 3, verify.TagLightGreen: 2, verify.TagYellow: 1,
			verify.TagOrange: 1, verify.TagRed: 3,
		},
		SelfVerified: 10, ThirdPartyVerified: 5, NeedsSelf: 2,
		QualifyingForThird: 4, QualifyingEstCostCents: 20,
		CreditsUsedTotal: 5, CreditsUsed30d: 5,
		BalanceCredits: &balance, ActiveRuns: 1, LastSelfRunAt: &lastRun,
	}

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Total int64 `json:"total"`
		ByTag struct {
			Green      int64 `json:"green"`
			LightGreen int64 `json:"light_green"`
			Yellow     int64 `json:"yellow"`
			Orange     int64 `json:"orange"`
			Red        int64 `json:"red"`
		} `json:"by_tag"`
		QualifyingForThirdParty int64      `json:"qualifying_for_third_party"`
		QualifyingEstCostCents  int64      `json:"qualifying_est_cost_cents"`
		BalanceCredits          *int64     `json:"balance_credits"`
		ActiveRuns              int64      `json:"active_runs"`
		LastSelfRunAt           *time.Time `json:"last_self_run_at"`
		LastThirdPartyRunAt     *time.Time `json:"last_third_party_run_at"`
	}](t, rec)

	if payload.Total != 10 {
		t.Errorf("total = %d", payload.Total)
	}
	if payload.ByTag.Green != 3 || payload.ByTag.Red != 3 || payload.ByTag.Yellow != 1 {
		t.Errorf("by_tag = %+v", payload.ByTag)
	}
	if payload.QualifyingForThirdParty != 4 || payload.QualifyingEstCostCents != 20 {
		t.Errorf("qualifying = %+v", payload)
	}
	if payload.BalanceCredits == nil || *payload.BalanceCredits != 900 {
		t.Errorf("balance = %v", payload.BalanceCredits)
	}
	if payload.LastSelfRunAt == nil {
		t.Error("the last self run timestamp is missing")
	}
	// A pass that has never run reports null rather than a zero time.
	if payload.LastThirdPartyRunAt != nil {
		t.Errorf("last third-party run = %v, want null", payload.LastThirdPartyRunAt)
	}
}

// An unreachable provider must not fail the page that reports on it.
func TestVerificationStatsWorksWithoutAProviderBalance(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.stats = verify.Stats{Total: 4, ByTag: map[verify.Tag]int64{verify.TagRed: 4}}

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	payload := decodeJSONBody[struct {
		BalanceCredits *int64 `json:"balance_credits"`
	}](t, rec)
	if payload.BalanceCredits != nil {
		t.Errorf("balance = %v, want null when the provider is unreachable", payload.BalanceCredits)
	}
}

func TestVerificationRoutesRequireTheAPIKey(t *testing.T) {
	handler, _ := newTestServer(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/verification/stats"},
		{http.MethodGet, "/api/v1/verification/emails"},
		{http.MethodGet, "/api/v1/verification/emails/" + testVerifyID.String()},
		{http.MethodPost, "/api/v1/verification/emails/" + testVerifyID.String() + "/self"},
		{http.MethodPost, "/api/v1/verification/emails/" + testVerifyID.String() + "/third-party"},
		{http.MethodPost, "/api/v1/verification/runs"},
		{http.MethodGet, "/api/v1/verification/runs"},
	}

	for _, route := range routes {
		rec := do(t, handler, route.method, route.path, "", withoutKey)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

// deref makes a failing score comparison print the number rather than the address.
func deref(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
