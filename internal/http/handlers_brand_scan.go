package httpapi

import (
	"net/http"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/exclusion"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/http/middleware"
)

// ScanBrands implements GET /exclusions/brand-scan.
func (s *Server) ScanBrands(w http.ResponseWriter, r *http.Request, params gen.ScanBrandsParams) {
	page, perPage := paginate(params.Page, params.PerPage)
	var requested *string
	if params.Sort != nil {
		v := string(*params.Sort)
		requested = &v
	}
	sort, err := resolveSort(requested, db.BrandScanSortKeys(), "locations:desc")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	in := exclusion.BrandScanInput{Sort: sort, Page: page, PerPage: perPage}
	f := &in.BrandScanFilter
	f.Q = params.Q
	if params.GroupBy != nil {
		f.GroupBy = string(*params.GroupBy)
	}
	if params.MinLocations != nil {
		f.MinLocations = *params.MinLocations
	}
	if params.MinCities != nil {
		f.MinCities = *params.MinCities
	}
	if params.MinStates != nil {
		f.MinStates = *params.MinStates
	}
	if params.Category != nil {
		f.Categories = *params.Category
	}
	if params.State != nil {
		f.States = *params.State
	}
	f.IncludeExcluded = params.IncludeExcluded != nil && *params.IncludeExcluded
	f.IncludeDismissed = params.IncludeDismissed != nil && *params.IncludeDismissed

	result, err := s.exclusions.BrandScan(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.BrandCandidate, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIBrandCandidate(row))
	}
	writeJSON(w, r, http.StatusOK, gen.BrandCandidateList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// ListBrandScanDismissals implements GET /exclusions/brand-scan/dismissals.
func (s *Server) ListBrandScanDismissals(w http.ResponseWriter, r *http.Request, params gen.ListBrandScanDismissalsParams) {
	page, perPage := paginate(params.Page, params.PerPage)
	var groupBy *string
	if params.GroupBy != nil {
		v := string(*params.GroupBy)
		groupBy = &v
	}
	result, err := s.exclusions.ListDismissals(r.Context(), groupBy, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.BrandScanDismissal, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIBrandScanDismissal(row))
	}
	writeJSON(w, r, http.StatusOK, gen.BrandScanDismissalList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// DismissBrandScanGroup implements POST /exclusions/brand-scan/dismissals.
func (s *Server) DismissBrandScanGroup(w http.ResponseWriter, r *http.Request) {
	var req gen.BrandScanDismissalCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	row, err := s.exclusions.Dismiss(r.Context(), exclusion.DismissalInput{
		GroupBy: string(req.GroupBy), Key: req.Key, Note: req.Note,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIBrandScanDismissal(row))
}

// UndismissBrandScanGroup implements DELETE /exclusions/brand-scan/dismissals/{id}.
func (s *Server) UndismissBrandScanGroup(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	if err := s.exclusions.Undismiss(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

// BulkCreateExclusions implements POST /exclusions/bulk.
func (s *Server) BulkCreateExclusions(w http.ResponseWriter, r *http.Request) {
	var req gen.ExclusionBulkCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := make([]exclusion.Input, 0, len(req.Items))
	for _, item := range req.Items {
		in = append(in, exclusionInput(item))
	}
	results, err := s.exclusions.CreateBulk(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := gen.ExclusionBulkResult{Results: make([]gen.ExclusionBulkItemResult, 0, len(results))}
	for i, res := range results {
		item := gen.ExclusionBulkItemResult{Index: i}
		switch {
		case res.Rule != nil:
			rule := toAPIExclusion(*res.Rule)
			item.Status, item.Exclusion = gen.ExclusionBulkItemResultStatusCreated, &rule
			out.Created++
		default:
			item.Status = bulkItemStatus(res.Err)
			if res.Err.Status >= 500 {
				middleware.LoggerFrom(r.Context()).Error("bulk exclusion item failed", "index", i, "error", res.Err.Error())
			}
			envelope := newErrorEnvelope(res.Err)
			item.Error = &envelope
		}
		out.Results = append(out.Results, item)
	}
	writeJSON(w, r, http.StatusOK, out)
}

func bulkItemStatus(err *apperr.Error) gen.ExclusionBulkItemResultStatus {
	switch err.Code {
	case apperr.CodeConflict:
		return gen.ExclusionBulkItemResultStatusDuplicate
	case apperr.CodeValidationFailed:
		return gen.ExclusionBulkItemResultStatusInvalid
	default:
		return gen.ExclusionBulkItemResultStatusFailed
	}
}

func toAPIBrandCandidate(row exclusion.BrandCandidate) gen.BrandCandidate {
	out := gen.BrandCandidate{
		GroupBy:           gen.BrandScanGroupBy(row.GroupBy),
		Key:               row.Key,
		DisplayName:       row.DisplayName,
		TopDomain:         row.TopDomain,
		Locations:         row.Locations,
		Cities:            row.Cities,
		States:            row.States,
		StateList:         row.StateList,
		DistinctNames:     row.DistinctNames,
		DistinctDomains:   row.DistinctDomains,
		TotalReviews:      row.TotalReviews,
		AvgRating:         row.AvgRating,
		ExcludedLocations: row.ExcludedLocations,
		ExistingExclusion: toAPIExclusionRef(row.Existing),
		SuggestedRule:     toAPIExclusionCreate(row.Suggested),
		SampleBusinesses:  make([]gen.BrandSampleBusiness, 0, len(row.Samples)),
	}
	if out.StateList == nil {
		out.StateList = []string{}
	}
	if row.DismissalID != nil {
		out.Dismissal = &gen.BrandScanDismissalRef{Id: *row.DismissalID, Note: row.DismissalNote}
	}
	for _, b := range row.Samples {
		out.SampleBusinesses = append(out.SampleBusinesses, gen.BrandSampleBusiness{
			Id: b.ID, Name: b.Name, City: b.City, State: b.State, Domain: b.Domain,
		})
	}
	return out
}

// toAPIExclusionCreate renders a rule input as the request body that creates it.
func toAPIExclusionCreate(in exclusion.Input) gen.GlobalExclusionCreate {
	mode := gen.ExclusionMatchMode(in.MatchMode)
	source := gen.ExclusionSource(in.Source)
	return gen.GlobalExclusionCreate{
		Kind: gen.ExclusionKind(in.Kind), Value: in.Value, MatchMode: &mode, Reason: in.Reason, Source: &source,
	}
}

func toAPIBrandScanDismissal(row dbgen.BrandScanDismissal) gen.BrandScanDismissal {
	return gen.BrandScanDismissal{
		Id: row.ID, GroupBy: gen.BrandScanGroupBy(row.GroupBy), Key: row.Key, Note: row.Note, CreatedAt: utc(row.CreatedAt),
	}
}
