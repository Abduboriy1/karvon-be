package httpapi

import (
	"net/http"

	"github.com/bory/karvon-be/internal/http/gen"
)

// ListSendingAccounts implements GET /sending-accounts.
func (s *Server) ListSendingAccounts(w http.ResponseWriter, r *http.Request, params gen.ListSendingAccountsParams) {
	views, err := s.campaigns.ListSendingAccounts(r.Context(), params.Status, params.Q)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.SendingAccount, 0, len(views))
	for _, view := range views {
		payload = append(payload, toAPISendingAccountView(view))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// SyncSendingAccounts implements POST /sending-accounts/sync.
func (s *Server) SyncSendingAccounts(w http.ResponseWriter, r *http.Request) {
	if err := s.campaigns.SyncSendingAccounts(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the refresh is queued, not done.
	writeJSON(w, r, http.StatusAccepted, nil)
}

// GetSendingAccount implements GET /sending-accounts/{id}.
func (s *Server) GetSendingAccount(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	view, daily, campaigns, err := s.campaigns.GetSendingAccount(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	dailyPayload := make([]gen.SendingAccountDaily, 0, len(daily))
	for _, row := range daily {
		dailyPayload = append(dailyPayload, toAPISendingAccountDaily(row))
	}
	campaignPayload := make([]gen.CampaignSummary, 0, len(campaigns))
	for _, row := range campaigns {
		campaignPayload = append(campaignPayload, toAPICampaignSummary(row))
	}

	writeJSON(w, r, http.StatusOK, gen.SendingAccountDetail{
		Account:   toAPISendingAccountView(view),
		Daily:     dailyPayload,
		Campaigns: campaignPayload,
	})
}
