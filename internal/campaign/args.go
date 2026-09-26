package campaign

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/queue"
)

// River job kinds. They are persisted in the queue table, so renaming one is a
// migration rather than a refactor.
const (
	KindLaunch                  = "campaign_launch"
	KindPushLeads               = "campaign_push_leads"
	KindActivate                = "campaign_activate"
	KindRemoveLead              = "campaign_remove_lead"
	KindProcessEvent            = "campaign_process_event"
	KindSyncCampaign            = "campaign_sync_campaign"
	KindSyncAll                 = "campaign_sync_all"
	KindSyncAccounts            = "campaign_sync_accounts"
	KindReplayWebhookEvents     = "campaign_replay_webhook_events"
	KindSyncLeadsFull           = "campaign_sync_leads_full"
	KindNewsletterPush          = "newsletter_push_member"
	KindNewsletterSyncMembers   = "newsletter_sync_members"
	KindNewsletterSyncAudiences = "newsletter_sync_audiences"
	KindExclusionSweep          = "campaign_exclusion_sweep"
)

// Newsletter push actions.
const (
	NewsletterActionUpsert      = "upsert"
	NewsletterActionUnsubscribe = "unsubscribe"
)

// Metadata tags a job with the campaign it belongs to, which is how
// archiving finds every pending job for a campaign.
func Metadata(id uuid.UUID) []byte {
	return []byte(fmt.Sprintf(`{"campaign_id":%q}`, id.String()))
}

// MetadataFilter is the JSON filter passed to river.JobListParams.Metadata.
func MetadataFilter(id uuid.UUID) string {
	raw, _ := json.Marshal(map[string]string{"campaign_id": id.String()})
	return string(raw)
}

func opts(q string, attempts int, meta []byte) river.InsertOpts {
	return river.InsertOpts{Queue: q, MaxAttempts: attempts, Metadata: meta, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// LaunchArgs creates the Instantly campaign and starts pushing leads.
type LaunchArgs struct {
	CampaignID uuid.UUID `json:"campaign_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (LaunchArgs) Kind() string { return KindLaunch }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a LaunchArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignPush, 5, Metadata(a.CampaignID))
}

// PushLeadsArgs pushes one batch of pending leads. Batch makes each batch its own
// unique job, so a retry of batch n cannot collide with batch n+1.
type PushLeadsArgs struct {
	CampaignID uuid.UUID `json:"campaign_id" river:"unique"`
	Batch      int       `json:"batch" river:"unique"`
}

// Kind implements river.JobArgs.
func (PushLeadsArgs) Kind() string { return KindPushLeads }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a PushLeadsArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignPush, 5, Metadata(a.CampaignID))
}

// ActivateArgs activates the Instantly campaign once every pending lead is pushed.
type ActivateArgs struct {
	CampaignID uuid.UUID `json:"campaign_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ActivateArgs) Kind() string { return KindActivate }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a ActivateArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignPush, 5, Metadata(a.CampaignID))
}

// RemoveLeadArgs deletes a pushed lead from Instantly after a suppression.
type RemoveLeadArgs struct {
	CampaignLeadID uuid.UUID `json:"campaign_lead_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (RemoveLeadArgs) Kind() string { return KindRemoveLead }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (RemoveLeadArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignPush, 3, nil)
}

// ExclusionSweepArgs applies a change to the global exclusion list to campaign
// leads: live leads a rule now covers stop (and leave Instantly when they were
// pushed), and leads a removed rule took out that never reached the provider go
// back to pending. The sweep reads the whole list, so any one job brings every
// lead up to date; ExclusionID only makes each change its own job.
type ExclusionSweepArgs struct {
	ExclusionID uuid.UUID `json:"exclusion_id" river:"unique"`
	Removed     bool      `json:"removed" river:"unique"`
}

// Kind implements river.JobArgs.
func (ExclusionSweepArgs) Kind() string { return KindExclusionSweep }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ExclusionSweepArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignPush, 10, nil)
}

// ProcessEventArgs applies one stored provider event.
type ProcessEventArgs struct {
	ProviderEventID uuid.UUID `json:"provider_event_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ProcessEventArgs) Kind() string { return KindProcessEvent }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ProcessEventArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignEvents, 5, nil)
}

// SyncCampaignArgs reconciles one campaign with Instantly.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator pressing "sync now" carries a fresh id and therefore
// always runs rather than collapsing into a reconcile already in flight.
type SyncCampaignArgs struct {
	CampaignID uuid.UUID `json:"campaign_id" river:"unique"`
	RequestID  uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (SyncCampaignArgs) Kind() string { return KindSyncCampaign }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a SyncCampaignArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignSync, 3, Metadata(a.CampaignID))
}

// SyncAllArgs fans out a sync for every live campaign.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator-triggered run carries a fresh id and always runs.
type SyncAllArgs struct {
	RequestID uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (SyncAllArgs) Kind() string { return KindSyncAll }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (SyncAllArgs) InsertOpts() river.InsertOpts { return opts(queue.QueueCampaignSync, 1, nil) }

// SyncAccountsArgs mirrors the Instantly sending accounts.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator-triggered run carries a fresh id and always runs.
type SyncAccountsArgs struct {
	RequestID uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (SyncAccountsArgs) Kind() string { return KindSyncAccounts }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (SyncAccountsArgs) InsertOpts() river.InsertOpts { return opts(queue.QueueCampaignSync, 3, nil) }

// ReplayWebhookEventsArgs re-ingests failed Instantly deliveries.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator-triggered run carries a fresh id and always runs.
type ReplayWebhookEventsArgs struct {
	RequestID uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (ReplayWebhookEventsArgs) Kind() string { return KindReplayWebhookEvents }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ReplayWebhookEventsArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueCampaignSync, 3, nil)
}

// SyncLeadsFullArgs diffs every campaign's leads against Instantly.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator-triggered run carries a fresh id and always runs.
type SyncLeadsFullArgs struct {
	RequestID uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (SyncLeadsFullArgs) Kind() string { return KindSyncLeadsFull }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (SyncLeadsFullArgs) InsertOpts() river.InsertOpts { return opts(queue.QueueCampaignSync, 3, nil) }

// NewsletterPushArgs pushes one subscription to Mailchimp.
//
// Subscription and action are unique together, so a double click cannot queue the
// same push twice. RequestID is what an explicit retry carries: without it the
// retry would be deduplicated against the attempt that already failed.
type NewsletterPushArgs struct {
	SubscriptionID uuid.UUID `json:"subscription_id" river:"unique"`
	Action         string    `json:"action" river:"unique"`
	RequestID      uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (NewsletterPushArgs) Kind() string { return KindNewsletterPush }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (NewsletterPushArgs) InsertOpts() river.InsertOpts { return opts(queue.QueueNewsletter, 5, nil) }

// NewsletterSyncMembersArgs mirrors member status from Mailchimp.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator-triggered run carries a fresh id and always runs.
type NewsletterSyncMembersArgs struct {
	RequestID uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (NewsletterSyncMembersArgs) Kind() string { return KindNewsletterSyncMembers }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (NewsletterSyncMembersArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueNewsletter, 3, nil)
}

// NewsletterSyncAudiencesArgs mirrors the audiences from Mailchimp.
//
// A zero RequestID is the periodic pass, which is unique by args and so never
// queues twice; an operator-triggered run carries a fresh id and always runs.
type NewsletterSyncAudiencesArgs struct {
	RequestID uuid.UUID `json:"request_id,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (NewsletterSyncAudiencesArgs) Kind() string { return KindNewsletterSyncAudiences }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (NewsletterSyncAudiencesArgs) InsertOpts() river.InsertOpts {
	return opts(queue.QueueNewsletter, 3, nil)
}
