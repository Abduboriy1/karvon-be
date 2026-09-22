package verify

import (
	"context"
	"errors"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/verify/provider"
)

// ProviderHealth is one provider's readiness, as the settings page shows it.
type ProviderHealth struct {
	Provider Key
	Label    string
	Stage    string
	// Weighted reports whether it takes part in the weighted free score.
	Weighted bool
	// Toggleable reports whether it can be switched off.
	Toggleable bool
	// Description says what the provider determines.
	Description string
	// Enabled is the current setting.
	Enabled bool
	// Weight is the configured percentage, 0 for the paid provider.
	Weight int
	// Healthy is nil when readiness cannot be determined — a provider that needs
	// no probing, such as the local pipeline or the compiled-in library. The UI
	// must show "not applicable" rather than a green tick in that case.
	Healthy *bool
	// Error is why a probe failed, empty when it did not.
	Error string
}

// SettingsView is everything GET /verification/settings returns: the editable
// policy, the catalogue the UI renders controls from, and live provider readiness.
type SettingsView struct {
	Settings  Settings
	Providers []ProviderHealth
	UpdatedAt *time.Time
	// FreeMaxScore is the ceiling on the weighted score, so the UI can explain why
	// three perfect scores do not make 100.
	FreeMaxScore int
	// WeightTotal is what the weights must add up to.
	WeightTotal int
}

// SettingsView assembles the settings payload, probing every provider that can be
// probed. A probe failure is reported as unhealthy rather than failing the request:
// the whole point of this endpoint is to tell the operator that Reacher is down.
func (s *Service) SettingsView(ctx context.Context) (SettingsView, error) {
	if s.settings == nil {
		return SettingsView{}, apperr.Internal(errNoSettingsStore)
	}
	settings, err := s.settings.Load(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	return SettingsView{
		Settings:     settings,
		Providers:    s.providerHealth(ctx, settings),
		UpdatedAt:    s.settings.UpdatedAt(ctx),
		FreeMaxScore: FreeMaxScore,
		WeightTotal:  WeightTotal,
	}, nil
}

// SaveSettings validates and persists new settings, returning the same view the
// getter does so the client needs exactly one round trip.
func (s *Service) SaveSettings(ctx context.Context, in Settings) (SettingsView, error) {
	if s.settings == nil {
		return SettingsView{}, apperr.Internal(errNoSettingsStore)
	}
	saved, err := s.settings.Save(ctx, in)
	if err != nil {
		return SettingsView{}, err
	}
	return SettingsView{
		Settings:     saved,
		Providers:    s.providerHealth(ctx, saved),
		UpdatedAt:    s.settings.UpdatedAt(ctx),
		FreeMaxScore: FreeMaxScore,
		WeightTotal:  WeightTotal,
	}, nil
}

// providerHealth renders the catalogue with the current settings and a readiness
// probe for each provider that supports one.
func (s *Service) providerHealth(ctx context.Context, settings Settings) []ProviderHealth {
	probes := map[Key]error{}
	if s.stage != nil {
		// Bound the probes: a hung provider must not hold the settings page open.
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		probes = s.stage.Health(probeCtx)
	}

	out := make([]ProviderHealth, 0, len(Catalog))
	for _, info := range Catalog {
		entry := ProviderHealth{
			Provider:    info.Key,
			Label:       info.Label,
			Stage:       info.Stage,
			Weighted:    info.Weighted,
			Toggleable:  info.Toggleable,
			Description: info.Description,
			Enabled:     settings.IsEnabled(info.Key),
			Weight:      settings.Weight(info.Key),
		}

		switch info.Key {
		case provider.KeyPaid:
			// The paid provider's readiness is whether a source with a usable key
			// exists, which the Sources page already owns; reuse that answer.
			source, err := s.verifierSource(ctx)
			healthy := err == nil && requireUsableVerifier(source) == nil
			entry.Healthy = &healthy
			if !healthy {
				entry.Error = "no enabled email verifier with an API key is configured"
			}
		default:
			if probeErr, probed := probes[info.Key]; probed {
				healthy := probeErr == nil
				entry.Healthy = &healthy
				if probeErr != nil {
					entry.Error = probeErr.Error()
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

// errNoSettingsStore is a wiring mistake rather than a runtime condition: the graph
// in package app always supplies a store.
var errNoSettingsStore = errors.New("verify: the service was built without a settings store")
