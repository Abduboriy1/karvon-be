package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/verify"
)

// GetVerificationStats implements GET /verification/stats.
func (s *Server) GetVerificationStats(w http.ResponseWriter, r *http.Request) {
	result, err := s.verification.Stats(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationStats(result))
}

// GetVerificationSettings implements GET /verification/settings.
func (s *Server) GetVerificationSettings(w http.ResponseWriter, r *http.Request) {
	view, err := s.verification.SettingsView(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationSettings(view))
}

// UpdateVerificationSettings implements PUT /verification/settings.
func (s *Server) UpdateVerificationSettings(w http.ResponseWriter, r *http.Request) {
	var body gen.VerificationSettingsUpdate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	view, err := s.verification.SaveSettings(r.Context(), toDomainSettings(body))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationSettings(view))
}

// ListVerifications implements GET /verification/emails.
func (s *Server) ListVerifications(w http.ResponseWriter, r *http.Request, params gen.ListVerificationsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.VerificationFilter{
		MinScore: params.MinScore,
		MaxScore: params.MaxScore,
		HasTypo:  params.HasTypo,
		Q:        params.Q,
	}
	if params.Tag != nil {
		for _, tag := range *params.Tag {
			filter.Tags = append(filter.Tags, string(tag))
		}
	}
	if params.Pass2Status != nil {
		status := string(*params.Pass2Status)
		filter.Pass2Status = &status
	}
	if params.BusinessId != nil {
		filter.BusinessIDs = []uuid.UUID{*params.BusinessId}
	}
	filter.JobID = params.JobId
	if params.IncludeSuppressed != nil {
		filter.IncludeSuppressed = *params.IncludeSuppressed
	}
	filter.Excluded = params.Excluded
	// "Needs third party" means exactly what a paid run would pick up: inside the
	// paid band, never yet sent to a third party, and not globally excluded.
	if params.NeedsThirdParty != nil && *params.NeedsThirdParty {
		notExcluded := false
		filter.Excluded = &notExcluded
		settings := s.verification.Settings(r.Context())
		complete, notSent := true, false
		minScore, maxScore := settings.PaidMinScore, settings.PaidThreshold
		filter.FreeComplete = &complete
		filter.MinFreeScore = &minScore
		filter.MaxFreeScore = &maxScore
		filter.ThirdPartySent = &notSent
	}

	sort, err := resolveSortParam(params.Sort, db.VerificationSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.verification.List(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.EmailVerification, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIVerification(row))
	}
	writeJSON(w, r, http.StatusOK, gen.EmailVerificationList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// GetVerification implements GET /verification/emails/{id}.
func (s *Server) GetVerification(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	detail, err := s.verification.Get(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationDetail(detail))
}

// VerifyEmailSelf implements POST /verification/emails/{id}/self.
func (s *Server) VerifyEmailSelf(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	s.verifyOne(w, r, id, verify.PassSelf)
}

// VerifyEmailThirdParty implements POST /verification/emails/{id}/third-party.
func (s *Server) VerifyEmailThirdParty(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	s.verifyOne(w, r, id, verify.PassThirdParty)
}

func (s *Server) verifyOne(w http.ResponseWriter, r *http.Request, id uuid.UUID, pass verify.Pass) {
	run, err := s.verification.VerifyOne(r.Context(), id, pass)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the work is queued, not done.
	writeJSON(w, r, http.StatusAccepted, toAPIVerificationRun(run))
}

// ApplyTypoSuggestion implements POST /verification/emails/{id}/apply-typo.
func (s *Server) ApplyTypoSuggestion(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	detail, err := s.verification.ApplyTypo(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationDetail(detail))
}

// EstimateVerificationRun implements POST /verification/runs/estimate.
func (s *Server) EstimateVerificationRun(w http.ResponseWriter, r *http.Request) {
	var body gen.VerificationRunEstimateRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	estimate, err := s.verification.EstimateRun(r.Context(),
		verify.Pass(body.Pass), toRunFilter(body.Filter))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIRunEstimate(estimate))
}

// CreateVerificationRun implements POST /verification/runs.
func (s *Server) CreateVerificationRun(w http.ResponseWriter, r *http.Request) {
	var body gen.VerificationRunCreate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	run, err := s.verification.CreateRun(r.Context(), verify.CreateRunInput{
		Pass:         verify.Pass(body.Pass),
		Filter:       toRunFilter(body.Filter),
		MaxCostCents: body.MaxCostCents,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIVerificationRun(run))
}

// ListVerificationRuns implements GET /verification/runs.
func (s *Server) ListVerificationRuns(w http.ResponseWriter, r *http.Request, params gen.ListVerificationRunsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	var pass, status *string
	if params.Pass != nil {
		value := string(*params.Pass)
		pass = &value
	}
	if params.Status != nil {
		value := string(*params.Status)
		status = &value
	}

	result, err := s.verification.ListRuns(r.Context(), pass, status, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.VerificationRun, 0, len(result.Runs))
	for _, run := range result.Runs {
		data = append(data, toAPIVerificationRun(run))
	}
	writeJSON(w, r, http.StatusOK, gen.VerificationRunList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// GetVerificationRun implements GET /verification/runs/{id}.
func (s *Server) GetVerificationRun(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	run, err := s.verification.GetRun(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationRun(run))
}

// CancelVerificationRun implements POST /verification/runs/{id}/cancel.
func (s *Server) CancelVerificationRun(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	run, err := s.verification.CancelRun(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIVerificationRun(run))
}

// toRunFilter maps the wire filter onto the domain one, defaulting the scope.
func toRunFilter(in *gen.VerificationRunFilter) verify.RunFilter {
	out := verify.RunFilter{Scope: verify.ScopeAll}
	if in == nil {
		return out
	}
	if in.Scope != nil {
		out.Scope = verify.RunScope(*in.Scope)
	}
	if in.Ids != nil {
		out.IDs = *in.Ids
	}
	if in.BusinessIds != nil {
		out.BusinessIDs = *in.BusinessIds
	}
	out.JobID = in.JobId
	if in.Tags != nil {
		for _, tag := range *in.Tags {
			out.Tags = append(out.Tags, string(tag))
		}
	}
	out.MinScore = in.MinScore
	if in.IncludeSuppressed != nil {
		out.IncludeSuppressed = *in.IncludeSuppressed
	}
	out.StaleAfterDays = in.StaleAfterDays
	return out
}
