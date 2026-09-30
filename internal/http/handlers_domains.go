package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/registrar"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

// domainSettingsRequest mirrors the spec's DomainSettingsUpdate. account_id and
// api_token are decoded as raw JSON so "field absent" (keep) can be told apart from
// "field is null" (clear).
type domainSettingsRequest struct {
	AccountID json.RawMessage `json:"account_id"`
	APIToken  json.RawMessage `json:"api_token"`
	Enabled   *bool           `json:"enabled"`
}

// GetDomainSettings implements GET /domains/settings.
func (s *Server) GetDomainSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.domains.Settings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIDomainSettings(settings))
}

// UpdateDomainSettings implements PUT /domains/settings.
func (s *Server) UpdateDomainSettings(w http.ResponseWriter, r *http.Request) {
	var req domainSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := registrar.SettingsInput{Enabled: req.Enabled}
	var err error
	if in.AccountIDPresent, in.AccountID, err = optionalString(req.AccountID); err != nil {
		WriteError(w, r, err)
		return
	}
	if in.TokenPresent, in.Token, err = optionalString(req.APIToken); err != nil {
		WriteError(w, r, err)
		return
	}
	settings, err := s.domains.SaveSettings(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIDomainSettings(settings))
}

// TestDomainConnection implements POST /domains/settings/test.
func (s *Server) TestDomainConnection(w http.ResponseWriter, r *http.Request) {
	result, err := s.domains.TestConnection(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.DomainConnectionTest{Ok: result.OK, TestedAt: utc(result.TestedAt)})
}

// SearchDomains implements GET /domains/search.
func (s *Server) SearchDomains(w http.ResponseWriter, r *http.Request, params gen.SearchDomainsParams) {
	in := registrar.SearchInput{Query: params.Q}
	if params.Limit != nil {
		in.Limit = *params.Limit
	}
	if params.Extensions != nil {
		in.Extensions = *params.Extensions
	}
	offers, err := s.domains.Search(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.DomainOfferList{Data: toAPIDomainOffers(offers)})
}

// CheckDomains implements POST /domains/check.
func (s *Server) CheckDomains(w http.ResponseWriter, r *http.Request) {
	var req gen.DomainCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	offers, err := s.domains.Check(r.Context(), req.Domains)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.DomainOfferList{Data: toAPIDomainOffers(offers)})
}

// ListDomainPurchases implements GET /domains/purchases.
func (s *Server) ListDomainPurchases(w http.ResponseWriter, r *http.Request, params gen.ListDomainPurchasesParams) {
	page, perPage := paginate(params.Page, params.PerPage)
	result, err := s.domains.ListPurchases(r.Context(), page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.DomainPurchase, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIDomainPurchase(row))
	}
	writeJSON(w, r, http.StatusOK, gen.DomainPurchaseList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// CreateDomainPurchase implements POST /domains/purchases. It answers 202: the
// purchase is queued, and the registrations happen in the background.
func (s *Server) CreateDomainPurchase(w http.ResponseWriter, r *http.Request) {
	var req gen.DomainPurchaseCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := registrar.PurchaseInput{
		Confirm:   req.Confirm,
		AutoRenew: req.AutoRenew != nil && *req.AutoRenew,
		Domains:   make([]registrar.PurchaseDomain, 0, len(req.Domains)),
	}
	for _, line := range req.Domains {
		in.Domains = append(in.Domains, registrar.PurchaseDomain{Name: line.Name, ExpectedCostCents: line.ExpectedCostCents})
	}
	purchase, err := s.domains.CreatePurchase(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, toAPIDomainPurchase(purchase))
}

// GetDomainPurchase implements GET /domains/purchases/{id}.
func (s *Server) GetDomainPurchase(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	purchase, err := s.domains.GetPurchase(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIDomainPurchase(purchase))
}

// ListDomainRegistrations implements GET /domains/registrations.
func (s *Server) ListDomainRegistrations(w http.ResponseWriter, r *http.Request) {
	regs, err := s.domains.ListRegistrations(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.DomainRegistration, 0, len(regs))
	for _, reg := range regs {
		data = append(data, toAPIDomainRegistration(reg))
	}
	writeJSON(w, r, http.StatusOK, gen.DomainRegistrationList{Data: data})
}

// GetDomainRegistration implements GET /domains/registrations/{domain}.
func (s *Server) GetDomainRegistration(w http.ResponseWriter, r *http.Request, domain gen.DomainPath) {
	reg, err := s.domains.GetRegistration(r.Context(), domain)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIDomainRegistration(reg))
}

// UpdateDomainRegistration implements PATCH /domains/registrations/{domain}.
func (s *Server) UpdateDomainRegistration(w http.ResponseWriter, r *http.Request, domain gen.DomainPath) {
	var req gen.DomainRegistrationUpdate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	reg, pending, err := s.domains.UpdateRegistration(r.Context(), domain, req.AutoRenew)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if pending {
		status = http.StatusAccepted
	}
	writeJSON(w, r, status, toAPIDomainRegistration(reg))
}

// optionalString decodes a nullable string field: absent, null, or a value.
func optionalString(raw json.RawMessage) (bool, *string, error) {
	if len(raw) == 0 {
		return false, nil, nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, nil, badRequestFromDecodeError(err)
	}
	return true, value, nil
}

func toAPIDomainSettings(in registrar.Settings) gen.DomainSettings {
	return gen.DomainSettings{
		SourceId:              in.SourceID,
		AccountId:             in.AccountID,
		HasToken:              in.HasToken,
		Enabled:               in.Enabled,
		Ready:                 in.Ready(),
		LastTestedAt:          utcPtr(in.LastTestedAt),
		LastTestOk:            in.LastTestOK,
		MaxDomainsPerPurchase: registrar.MaxDomainsPerPurchase,
		MaxCheckDomains:       cloudflare.MaxCheckDomains,
	}
}

func toAPIDomainOffers(offers []registrar.Offer) []gen.DomainOffer {
	out := make([]gen.DomainOffer, 0, len(offers))
	for _, offer := range offers {
		item := gen.DomainOffer{
			Name:        offer.Name,
			Registrable: offer.Registrable,
			Purchasable: offer.Purchasable,
			Reason:      offer.Reason,
		}
		if offer.Tier != "" {
			tier := offer.Tier
			item.Tier = &tier
		}
		if p := offer.Pricing; p != nil {
			item.Pricing = &gen.DomainPricing{
				Currency:              p.Currency,
				RegistrationCost:      p.RegistrationCost,
				RenewalCost:           p.RenewalCost,
				RegistrationCostCents: p.RegistrationCostCents,
				RenewalCostCents:      p.RenewalCostCents,
			}
		}
		out = append(out, item)
	}
	return out
}

func toAPIDomainPurchase(in registrar.Purchase) gen.DomainPurchase {
	out := gen.DomainPurchase{
		Id:               in.ID,
		Status:           gen.DomainPurchaseStatus(in.Status),
		AutoRenew:        in.AutoRenew,
		Currency:         in.Currency,
		ItemCount:        int(in.ItemCount),
		QuotedTotalCents: in.QuotedTotalCents,
		Error:            in.Error,
		CreatedAt:        utc(in.CreatedAt),
		StartedAt:        utcPtr(in.StartedAt),
		FinishedAt:       utcPtr(in.FinishedAt),
		Items:            make([]gen.DomainPurchaseItem, 0, len(in.Items)),
	}
	for _, item := range in.Items {
		switch item.Status {
		case registrar.ItemSucceeded:
			out.SucceededCount++
			if item.CostCents != nil {
				out.ChargedTotalCents += *item.CostCents
			} else {
				out.ChargedTotalCents += item.QuotedCostCents
			}
		case registrar.ItemFailed:
			out.FailedCount++
		case registrar.ItemActionRequired:
			out.ActionRequiredCount++
		default:
			out.InFlightCount++
		}
		out.Items = append(out.Items, toAPIDomainPurchaseItem(item))
	}
	return out
}

func toAPIDomainPurchaseItem(in dbgen.DomainPurchaseItem) gen.DomainPurchaseItem {
	return gen.DomainPurchaseItem{
		Id:               in.ID,
		DomainName:       in.DomainName,
		Position:         int(in.Position),
		Status:           gen.DomainPurchaseItemStatus(in.Status),
		QuotedCostCents:  in.QuotedCostCents,
		CostCents:        in.CostCents,
		RenewalCostCents: in.RenewalCostCents,
		RegisterAttempts: int(in.RegisterAttempts),
		ErrorCode:        in.ErrorCode,
		ErrorMessage:     in.ErrorMessage,
		RegisteredAt:     utcPtr(in.RegisteredAt),
		ExpiresAt:        utcPtr(in.ExpiresAt),
		UpdatedAt:        utc(in.UpdatedAt),
	}
}

func toAPIDomainRegistration(in registrar.Registration) gen.DomainRegistration {
	return gen.DomainRegistration{
		DomainName:  in.DomainName,
		Status:      in.Status,
		CreatedAt:   utcPtr(in.CreatedAt),
		ExpiresAt:   utcPtr(in.ExpiresAt),
		AutoRenew:   in.AutoRenew,
		Locked:      in.Locked,
		PrivacyMode: in.PrivacyMode,
	}
}
