package app

import (
	"time"

	"github.com/bory/karvon-be/internal/campaign/ai"
	campaignjobs "github.com/bory/karvon-be/internal/campaign/jobs"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/verify"
)

// buildCampaign assembles the campaign module: the two provider factories over
// the encrypted keys in the sources table, the AI generator (the manual ChatGPT
// flow unless an OpenAI key is configured), the service the HTTP layer talks to,
// and the worker dependency bundle.
func (a *App) buildCampaign(cipher *crypto.Cipher) (*campaignjobs.Deps, *service.Service) {
	var instantlyFactory instantly.Factory = instantly.NewFactory(cipher, instantly.FactoryConfig{
		BaseURL: a.cfg.InstantlyBaseURL,
		Timeout: a.cfg.InstantlyTimeout,
		Log:     a.log,
	})
	if a.opts.instantlyFactory != nil {
		instantlyFactory = a.opts.instantlyFactory
	}

	var mailchimpFactory mailchimp.Factory = mailchimp.NewFactory(cipher, mailchimp.FactoryConfig{
		BaseURL:     a.cfg.MailchimpBaseURL,
		Timeout:     a.cfg.MailchimpTimeout,
		Concurrency: a.cfg.MailchimpConcurrency,
		Log:         a.log,
	})
	if a.opts.mailchimpFactory != nil {
		mailchimpFactory = a.opts.mailchimpFactory
	}

	// A ChatGPT subscription grants no API access, so the manual copy-and-paste
	// flow is the default and the API provider only appears with its own key.
	var generator ai.Provider = ai.NewManual()
	switch {
	case a.opts.aiProvider != nil:
		generator = a.opts.aiProvider
	case a.cfg.OpenAIAPIKey != "":
		generator = ai.NewOpenAI(ai.OpenAIConfig{
			APIKey:  a.cfg.OpenAIAPIKey,
			BaseURL: a.cfg.OpenAIBaseURL,
			Model:   a.cfg.OpenAIModel,
			Timeout: a.cfg.OpenAITimeout,
			Log:     a.log,
		})
	}

	svc := service.NewService(a.store, cipher, instantlyFactory, mailchimpFactory, generator, nil, a.log,
		service.Config{
			PublicBaseURL: a.cfg.PublicBaseURL,
			MaxImport:     a.cfg.CampaignMaxImport,
			LeadBatch:     a.cfg.InstantlyLeadBatch,
		})

	clock := a.opts.campaignClock
	if clock != nil {
		svc.SetClock(clock)
	} else {
		clock = func() time.Time { return time.Now().UTC() }
	}

	deps := campaignjobs.NewDeps(campaignjobs.Deps{
		Store:   a.store,
		Service: svc,
		Limiter: verify.NewRateLimiter(a.cfg.InstantlyRPS),
		Log:     a.log,
		Now:     clock,
		Config: campaignjobs.Config{
			LeadBatch:    a.cfg.InstantlyLeadBatch,
			LeadBatchGap: a.cfg.InstantlyLeadBatchGap,
		},
	})

	a.log.Info("campaigns ready",
		"ai_provider", generator.Name(),
		"ai_mode", generator.Mode(),
		"webhooks", a.cfg.PublicBaseURL != "",
		"instantly_rps", a.cfg.InstantlyRPS)

	return deps, svc
}
