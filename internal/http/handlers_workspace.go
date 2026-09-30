package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/workspace"
	"github.com/bory/karvon-be/internal/workspace/google"
)

// workspaceSettingsRequest mirrors the spec's WorkspaceSettingsUpdate. admin_email and
// service_account_key are decoded as raw JSON so "field absent" (keep) can be told
// apart from "field is null" (clear).
type workspaceSettingsRequest struct {
	AdminEmail        json.RawMessage `json:"admin_email"`
	ServiceAccountKey json.RawMessage `json:"service_account_key"`
	Enabled           *bool           `json:"enabled"`
}

// GetWorkspaceSettings implements GET /workspace/settings.
func (s *Server) GetWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.mailboxes.Settings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIWorkspaceSettings(settings))
}

// UpdateWorkspaceSettings implements PUT /workspace/settings.
func (s *Server) UpdateWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	var req workspaceSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := workspace.SettingsInput{Enabled: req.Enabled}
	var err error
	if in.AdminEmailPresent, in.AdminEmail, err = optionalString(req.AdminEmail); err != nil {
		WriteError(w, r, err)
		return
	}
	if in.KeyPresent, in.Key, err = optionalString(req.ServiceAccountKey); err != nil {
		WriteError(w, r, err)
		return
	}
	settings, err := s.mailboxes.SaveSettings(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIWorkspaceSettings(settings))
}

// TestWorkspaceConnection implements POST /workspace/settings/test.
func (s *Server) TestWorkspaceConnection(w http.ResponseWriter, r *http.Request) {
	result, err := s.mailboxes.TestConnection(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.WorkspaceConnectionTest{
		Ok:            result.OK,
		TestedAt:      utc(result.TestedAt),
		PrimaryDomain: result.PrimaryDomain,
		DomainCount:   result.DomainCount,
	})
}

// ListWorkspaceDomains implements GET /workspace/domains.
func (s *Server) ListWorkspaceDomains(w http.ResponseWriter, r *http.Request, params gen.ListWorkspaceDomainsParams) {
	page, perPage := paginate(params.Page, params.PerPage)
	result, err := s.mailboxes.ListSetups(r.Context(), page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.WorkspaceDomain, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIWorkspaceDomain(row))
	}
	writeJSON(w, r, http.StatusOK, gen.WorkspaceDomainList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// CreateWorkspaceDomain implements POST /workspace/domains. It answers 202: the setup
// is queued, and happens in the background.
func (s *Server) CreateWorkspaceDomain(w http.ResponseWriter, r *http.Request) {
	var req gen.WorkspaceDomainCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := workspace.SetupInput{Domain: req.Domain, Confirm: req.Confirm, Mailboxes: toMailboxInputs(req.Mailboxes)}
	setup, err := s.mailboxes.CreateSetup(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, toAPIWorkspaceDomain(setup))
}

// GetWorkspaceDomain implements GET /workspace/domains/{domain}.
func (s *Server) GetWorkspaceDomain(w http.ResponseWriter, r *http.Request, domain gen.DomainPath) {
	setup, err := s.mailboxes.GetSetup(r.Context(), domain)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIWorkspaceDomain(setup))
}

// RetryWorkspaceDomain implements POST /workspace/domains/{domain}/retry.
func (s *Server) RetryWorkspaceDomain(w http.ResponseWriter, r *http.Request, domain gen.DomainPath) {
	setup, err := s.mailboxes.RetrySetup(r.Context(), domain)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, toAPIWorkspaceDomain(setup))
}

// PublishWorkspaceDkim implements PUT /workspace/domains/{domain}/dkim.
func (s *Server) PublishWorkspaceDkim(w http.ResponseWriter, r *http.Request, domain gen.DomainPath) {
	var req gen.WorkspaceDkimUpdate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	selector := ""
	if req.Selector != nil {
		selector = *req.Selector
	}
	setup, err := s.mailboxes.PublishDKIM(r.Context(), domain, selector, req.Value)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIWorkspaceDomain(setup))
}

// AddWorkspaceMailboxes implements POST /workspace/domains/{domain}/mailboxes.
func (s *Server) AddWorkspaceMailboxes(w http.ResponseWriter, r *http.Request, domain gen.DomainPath) {
	var req gen.WorkspaceMailboxesAdd
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	setup, err := s.mailboxes.AddMailboxes(r.Context(), domain, toMailboxInputs(req.Mailboxes), req.Confirm)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, toAPIWorkspaceDomain(setup))
}

// DeleteWorkspaceMailbox implements DELETE /workspace/mailboxes/{id}.
func (s *Server) DeleteWorkspaceMailbox(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	if err := s.mailboxes.DeleteMailbox(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

// ConnectWorkspaceMailboxToInstantly implements POST /workspace/mailboxes/{id}/instantly.
func (s *Server) ConnectWorkspaceMailboxToInstantly(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req gen.WorkspaceInstantlyConnect
	if err := decodeJSONIfPresent(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	conn, err := s.mailboxes.ConnectInstantly(r.Context(), id, req.Warmup != nil && *req.Warmup)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.WorkspaceInstantlyConnection{
		AuthUrl:   conn.AuthURL,
		ExpiresAt: utc(conn.ExpiresAt),
		Mailbox:   toAPIWorkspaceMailbox(conn.Mailbox),
	})
}

func toMailboxInputs(in []gen.WorkspaceMailboxInput) []workspace.MailboxInput {
	out := make([]workspace.MailboxInput, 0, len(in))
	for _, mb := range in {
		out = append(out, workspace.MailboxInput{LocalPart: mb.LocalPart, GivenName: mb.GivenName, FamilyName: mb.FamilyName})
	}
	return out
}

// GetWorkspaceMailboxCredentials implements GET /workspace/mailboxes/{id}/credentials.
func (s *Server) GetWorkspaceMailboxCredentials(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	creds, err := s.mailboxes.MailboxCredentials(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, gen.WorkspaceMailboxCredentials{Email: creds.Email, Password: creds.Password})
}

func toAPIWorkspaceSettings(in workspace.Settings) gen.WorkspaceSettings {
	return gen.WorkspaceSettings{
		SourceId:               in.SourceID,
		AdminEmail:             in.AdminEmail,
		ServiceAccountEmail:    in.ServiceAccountEmail,
		ServiceAccountClientId: in.ServiceAccountClientID,
		HasKey:                 in.HasKey,
		Enabled:                in.Enabled,
		Ready:                  in.Ready(),
		LastTestedAt:           utcPtr(in.LastTestedAt),
		LastTestOk:             in.LastTestOK,
		Scopes:                 append([]string(nil), google.Scopes...),
		MaxMailboxesPerDomain:  workspace.MaxMailboxesPerDomain,
	}
}

func toAPIWorkspaceDomain(in workspace.Setup) gen.WorkspaceDomain {
	out := gen.WorkspaceDomain{
		Id:                in.ID,
		DomainName:        in.DomainName,
		Status:            gen.WorkspaceDomainStatus(in.Status),
		NextStep:          nextStep(in.WorkspaceDomain),
		VerificationToken: in.VerificationToken,
		DkimSelector:      in.DkimSelector,
		AddedAt:           utcPtr(in.AddedAt),
		DnsPublishedAt:    utcPtr(in.DnsPublishedAt),
		VerifiedAt:        utcPtr(in.VerifiedAt),
		DkimPublishedAt:   utcPtr(in.DkimPublishedAt),
		ErrorCode:         in.ErrorCode,
		ErrorMessage:      in.ErrorMessage,
		CreatedAt:         utc(in.CreatedAt),
		UpdatedAt:         utc(in.UpdatedAt),
		Mailboxes:         make([]gen.WorkspaceMailbox, 0, len(in.Mailboxes)),
	}
	for _, mb := range in.Mailboxes {
		out.Mailboxes = append(out.Mailboxes, toAPIWorkspaceMailbox(mb))
	}
	return out
}

func toAPIWorkspaceMailbox(in dbgen.WorkspaceMailbox) gen.WorkspaceMailbox {
	return gen.WorkspaceMailbox{
		Id:             in.ID,
		Email:          in.Email,
		GivenName:      in.GivenName,
		FamilyName:     in.FamilyName,
		Position:       int(in.Position),
		Status:         gen.WorkspaceMailboxStatus(in.Status),
		CreateAttempts: int(in.CreateAttempts),
		ErrorCode:      in.ErrorCode,
		ErrorMessage:   in.ErrorMessage,
		ProvisionedAt:  utcPtr(in.ProvisionedAt),
		UpdatedAt:      utc(in.UpdatedAt),

		InstantlyStatus:           (*gen.WorkspaceInstantlyStatus)(in.InstantlyStatus),
		InstantlyAccountId:        in.InstantlyAccountID,
		InstantlyError:            in.InstantlyError,
		InstantlySessionExpiresAt: utcPtr(in.InstantlySessionExpiresAt),
		InstantlyConnectedAt:      utcPtr(in.InstantlyConnectedAt),
		InstantlyWarmup:           in.InstantlyWarmup,
		InstantlyWarmupEnabledAt:  utcPtr(in.InstantlyWarmupEnabledAt),
	}
}

// nextStep says what a person has to do, for the states that wait on one.
func nextStep(in dbgen.WorkspaceDomain) *string {
	var step string
	switch in.Status {
	case workspace.DomainDKIMRequired:
		step = "In the Google Admin console open Apps → Google Workspace → Gmail → Authenticate email, pick " +
			in.DomainName + ", press Generate new record, and send the TXT value to PUT /workspace/domains/" +
			in.DomainName + "/dkim. Then press Start authentication."
	case workspace.DomainActive:
		step = "If authentication is not on yet, press Start authentication for " + in.DomainName +
			" in the Admin console (Apps → Google Workspace → Gmail → Authenticate email). Warm the mailboxes up before sending."
	case workspace.DomainFailed:
		step = "Fix what error_message describes, then POST /workspace/domains/" + in.DomainName + "/retry."
	default:
		return nil
	}
	return &step
}
