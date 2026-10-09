package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// freeSlotsBatch is how many Karvon leads one bulk delete names. Instantly takes
// up to 10000 per call; a smaller batch keeps each call and its stamping short.
const freeSlotsBatch = 500

// freeSlotsMaxRounds bounds the Karvon pass, so a lead that will not leave the
// filter can never keep a launch looping.
const freeSlotsMaxRounds = 100

// importedDeleteLimit is the most Instantly deletes in one bulk call.
const importedDeleteLimit = 10000

// freedSlots is what freeInstantlySlots deleted.
type freedSlots struct {
	Karvon   int
	Imported int
}

// freeInstantlySlots deletes the leads Instantly has finished with, so a launch's
// push does not run into the plan's uploaded-contacts cap. It takes:
//
//   - Karvon leads the "finished" cleanup scope matches, with the saved idle
//     window and replied setting. Each is stamped provider_removed_at, as a
//     cleanup run does, so the record of who was emailed survives.
//   - Every lead of a campaign started in Instantly's own app that Instantly
//     reports completed. Those leads were never mirrored here.
//
// Nothing in a draft, scheduled, active or paused campaign is touched. It is safe
// to run again: what is already gone matches nothing.
func freeInstantlySlots(ctx context.Context, d *Deps, client instantly.Client) (freedSlots, error) {
	var out freedSlots
	settings, err := d.Service.GetCleanupSettings(ctx)
	if err != nil {
		return out, err
	}
	policy := settings.CleanupPolicy
	policy.Scope = campaign.CleanupScopeFinished
	filter := service.CleanupFilterFor(policy, nil, d.Now())

	instantlyIDs := map[uuid.UUID]string{}
	for round := 0; round < freeSlotsMaxRounds; round++ {
		candidates, err := d.Store.ListCleanupCandidates(ctx, filter, freeSlotsBatch)
		if err != nil {
			return out, fmt.Errorf("campaign jobs: list finished leads: %w", err)
		}
		if len(candidates) == 0 {
			break
		}
		byCampaign := map[string][]uuid.UUID{}
		providerIDs := map[string][]string{}
		for _, c := range candidates {
			iid, ok := instantlyIDs[c.CampaignID]
			if !ok {
				camp, err := d.Store.GetCampaign(ctx, c.CampaignID)
				if err != nil {
					return out, fmt.Errorf("campaign jobs: load campaign: %w", err)
				}
				iid = campaign.Deref(camp.InstantlyCampaignID)
				instantlyIDs[c.CampaignID] = iid
			}
			byCampaign[iid] = append(byCampaign[iid], c.ID)
			providerIDs[iid] = append(providerIDs[iid], c.InstantlyLeadID)
		}
		for iid, leadIDs := range byCampaign {
			if err := d.Limiter.Wait(ctx); err != nil {
				return out, err
			}
			// Limit is the number of ids named, so the call can never take more
			// than those leads even if Instantly ignored the list.
			_, err := client.DeleteLeads(ctx, instantly.DeleteLeadsInput{CampaignID: iid, IDs: providerIDs[iid], Limit: len(leadIDs)})
			if err != nil && !errors.Is(err, provider.ErrNotFound) {
				return out, fmt.Errorf("campaign jobs: delete finished leads: %w", err)
			}
			for _, id := range leadIDs {
				lead, err := d.Store.GetCampaignLead(ctx, id)
				if err != nil {
					return out, fmt.Errorf("campaign jobs: load lead: %w", err)
				}
				if err := MarkRemovedFromProvider(ctx, d, lead, campaign.ProviderRemovedCleanup, uuid.NullUUID{}); err != nil {
					return out, err
				}
				out.Karvon++
			}
		}
	}

	imported, err := d.Store.ListCampaignsByStatus(ctx, []string{campaign.CampaignCompleted})
	if err != nil {
		return out, fmt.Errorf("campaign jobs: list completed campaigns: %w", err)
	}
	for _, camp := range imported {
		if camp.Source != campaign.CampaignSourceInstantly || camp.InstantlyCampaignID == nil {
			continue
		}
		for {
			if err := d.Limiter.Wait(ctx); err != nil {
				return out, err
			}
			n, err := client.DeleteLeads(ctx, instantly.DeleteLeadsInput{CampaignID: *camp.InstantlyCampaignID, Limit: importedDeleteLimit})
			if errors.Is(err, provider.ErrNotFound) {
				break
			}
			if err != nil {
				return out, fmt.Errorf("campaign jobs: delete leads of %q: %w", camp.Name, err)
			}
			out.Imported += n
			if n < importedDeleteLimit {
				break
			}
		}
		if err := d.Store.SetImportedCampaignCounts(ctx, dbgen.SetImportedCampaignCountsParams{ID: camp.ID, LeadsTotal: 0}); err != nil {
			return out, fmt.Errorf("campaign jobs: reset imported counts: %w", err)
		}
	}
	if out.Karvon+out.Imported > 0 {
		d.Log.Info("freed Instantly contact slots", "karvon_leads", out.Karvon, "imported_leads", out.Imported)
	}
	return out, nil
}
