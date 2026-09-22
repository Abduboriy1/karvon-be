package httpapi

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/consent"
	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
)

// The campaign area stores a fair amount of JSONB and a fair number of int32
// columns. These mappers are the single place where that shape is turned into the
// one the spec promises: documents are decoded into objects, never handed back as
// raw bytes; counters widen to the width the DTO declares; and a list field is
// always present, empty rather than null, so a client never has to nil-check it.

/* ------------------------------------------------------------- primitives */

// decodeJSONObject decodes a JSONB column into an object. An empty column, or one
// holding something that is not an object, becomes null rather than failing the
// request: the row's other fields still matter.
func decodeJSONObject(raw []byte) *map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil
	}
	return &out
}

// decodeIntArray decodes a JSONB array of integers, such as a campaign's step delays.
func decodeIntArray(raw []byte) *[]int {
	if len(raw) == 0 {
		return nil
	}
	var out []int
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil
	}
	return &out
}

// nullUUID maps a nullable id column onto the wire's nullable UUID.
func nullUUID(v uuid.NullUUID) *openapi_types.UUID {
	if !v.Valid {
		return nil
	}
	id := v.UUID
	return &id
}

// widenInt32 widens a nullable int32 column to the int the DTO declares.
func widenInt32(v *int32) *int {
	if v == nil {
		return nil
	}
	out := int(*v)
	return &out
}

// stringList guarantees a text[] column reaches the client as `[]`, never null.
func stringList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// uuidList is stringList for an optional uuid[] column.
func uuidList(v []uuid.UUID) *[]openapi_types.UUID {
	out := v
	if out == nil {
		out = []openapi_types.UUID{}
	}
	return &out
}

// counts guarantees a counts-by-key map is an object rather than null.
func counts(in map[string]int64) gen.CountsByKey {
	if in == nil {
		return gen.CountsByKey{}
	}
	return gen.CountsByKey(in)
}

// apiDate parses a `YYYY-MM-DD` day label from a grouped query.
func apiDate(day string) openapi_types.Date {
	parsed, err := time.Parse(openapi_types.DateFormat, day)
	if err != nil {
		return openapi_types.Date{}
	}
	return openapi_types.Date{Time: parsed}
}

// optString returns nil for an empty string, so an absent value is null rather than "".
func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// optInt returns nil for a zero count.
func optInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

/* -------------------------------------------------------------- campaigns */

func toAPICampaign(row db.CampaignRow) gen.Campaign {
	return gen.Campaign{
		Id:                     row.ID,
		Name:                   row.Name,
		Status:                 gen.CampaignStatus(row.Status),
		Brief:                  decodeJSONObject(row.Brief),
		Schedule:               decodeJSONObject(row.Schedule),
		Settings:               decodeJSONObject(row.Settings),
		Steps:                  int(row.Steps),
		StepDelays:             decodeIntArray(row.StepDelays),
		WeightsVersion:         int(row.WeightsVersion),
		InstantlyCampaignId:    row.InstantlyCampaignID,
		InstantlyStatus:        widenInt32(row.InstantlyStatus),
		InstantlySendingStatus: row.InstantlySendingState,
		LaunchedAt:             utcPtr(row.LaunchedAt),
		PausedAt:               utcPtr(row.PausedAt),
		CompletedAt:            utcPtr(row.CompletedAt),
		ArchivedAt:             utcPtr(row.ArchivedAt),
		LastSyncedAt:           utcPtr(row.LastSyncedAt),
		LastSyncError:          row.LastSyncError,
		Error:                  row.Error,
		LeadsTotal:             int(row.LeadsTotal),
		LeadsPushed:            int(row.LeadsPushed),
		CreatedAt:              utc(row.CreatedAt),
		UpdatedAt:              utc(row.UpdatedAt),
		Contacted:              row.Contacted,
		Replied:                row.Replied,
		Interested:             row.Interested,
		Bounced:                row.Bounced,
		Unsubscribed:           row.Unsubscribed,
		SendsTotal:             row.SendsTotal,
		SendsBounced:           row.SendsBounced,
		Eligible:               row.Eligible,
		Subscribed:             row.Subscribed,
		LastActivityAt:         utcPtr(row.LastActivityAt),
	}
}

func toAPICampaignDetail(detail campaignsvc.CampaignDetail) gen.CampaignDetail {
	row := detail.Campaign

	accounts := make([]gen.SendingAccount, 0, len(detail.SendingAccounts))
	for _, account := range detail.SendingAccounts {
		accounts = append(accounts, toAPISendingAccount(account))
	}
	variants := make([]gen.CampaignVariant, 0, len(detail.Variants))
	for _, variant := range detail.Variants {
		variants = append(variants, toAPICampaignVariant(variant))
	}

	return gen.CampaignDetail{
		Id:                     row.ID,
		Name:                   row.Name,
		Status:                 gen.CampaignStatus(row.Status),
		Brief:                  decodeJSONObject(row.Brief),
		Schedule:               decodeJSONObject(row.Schedule),
		Settings:               decodeJSONObject(row.Settings),
		Steps:                  int(row.Steps),
		StepDelays:             decodeIntArray(row.StepDelays),
		WeightsVersion:         int(row.WeightsVersion),
		InstantlyCampaignId:    row.InstantlyCampaignID,
		InstantlyStatus:        widenInt32(row.InstantlyStatus),
		InstantlySendingStatus: row.InstantlySendingState,
		LaunchedAt:             utcPtr(row.LaunchedAt),
		PausedAt:               utcPtr(row.PausedAt),
		CompletedAt:            utcPtr(row.CompletedAt),
		ArchivedAt:             utcPtr(row.ArchivedAt),
		LastSyncedAt:           utcPtr(row.LastSyncedAt),
		LastSyncError:          row.LastSyncError,
		Error:                  row.Error,
		LeadsTotal:             int(row.LeadsTotal),
		LeadsPushed:            int(row.LeadsPushed),
		CreatedAt:              utc(row.CreatedAt),
		UpdatedAt:              utc(row.UpdatedAt),
		Contacted:              row.Contacted,
		Replied:                row.Replied,
		Interested:             row.Interested,
		Bounced:                row.Bounced,
		Unsubscribed:           row.Unsubscribed,
		SendsTotal:             row.SendsTotal,
		SendsBounced:           row.SendsBounced,
		Eligible:               row.Eligible,
		Subscribed:             row.Subscribed,
		LastActivityAt:         utcPtr(row.LastActivityAt),

		Checklist:       toAPIChecklist(detail.Checklist),
		LeadCounts:      counts(detail.LeadCounts),
		StageCounts:     counts(detail.StageCounts),
		SendingAccounts: accounts,
		Variants:        variants,
	}
}

func toAPICampaignSummary(row dbgen.Campaign) gen.CampaignSummary {
	return gen.CampaignSummary{
		Id:     row.ID,
		Name:   row.Name,
		Status: gen.CampaignStatus(row.Status),
	}
}

func toAPIChecklist(in campaignsvc.Checklist) gen.CampaignChecklist {
	items := make([]gen.ChecklistItem, 0, len(in.Items))
	for _, item := range in.Items {
		items = append(items, gen.ChecklistItem{
			Key:      item.Key,
			Ok:       item.OK,
			Blocking: item.Blocking,
			Message:  item.Message,
		})
	}
	return gen.CampaignChecklist{Ready: in.Ready, Items: items}
}

func toAPICampaignVariant(row dbgen.ListCampaignVariantsRow) gen.CampaignVariant {
	return gen.CampaignVariant{
		CampaignId:       row.CampaignID,
		VariantId:        row.VariantID,
		Step:             int(row.Step),
		Weight:           int(row.Weight),
		AssignmentStatus: gen.CampaignVariantStatus(row.AssignmentStatus),
		AttachedAt:       utc(row.AttachedAt),
		Name:             row.Name,
		VariantStatus:    gen.ContentStatus(row.VariantStatus),
		SubjectTemplate:  row.SubjectTemplate,
		BodyTemplate:     row.BodyTemplate,
		ComponentIds:     uuidList(row.ComponentIds),
	}
}

/* ------------------------------------------------------------------ leads */

func toAPICampaignLead(row db.LeadRow) gen.CampaignLead {
	return gen.CampaignLead{
		Id:              row.ID,
		CampaignId:      row.CampaignID,
		ContactId:       row.ContactID,
		BusinessId:      nullUUID(row.BusinessID),
		Status:          gen.CampaignLeadStatus(row.Status),
		InstantlyLeadId: row.InstantlyLeadID,
		InstantlyStatus: widenInt32(row.InstantlyStatus),
		InterestStatus:  widenInt32(row.InterestStatus),
		InterestLabel:   row.InterestLabel,
		PushedAt:        utcPtr(row.PushedAt),
		PushAttempts:    int(row.PushAttempts),
		LastPushError:   row.LastPushError,
		LastContactedAt: utcPtr(row.LastContactedAt),
		LastOpenedAt:    utcPtr(row.LastOpenedAt),
		LastClickedAt:   utcPtr(row.LastClickedAt),
		LastRepliedAt:   utcPtr(row.LastRepliedAt),
		OpenCount:       int(row.OpenCount),
		ClickCount:      int(row.ClickCount),
		ReplyCount:      int(row.ReplyCount),
		CreatedAt:       utc(row.CreatedAt),
		UpdatedAt:       utc(row.UpdatedAt),
		Email:           row.Email,
		FirstName:       row.FirstName,
		LastName:        row.LastName,
		Company:         row.Company,
		Title:           row.Title,
		LifecycleStage:  gen.ContactStage(row.LifecycleStage),
		SuppressedAt:    utcPtr(row.SuppressedAt),
		VariantNames:    stringList(row.VariantNames),
		SendsTotal:      row.SendsTotal,
	}
}

// toAPICampaignLeadBase renders a lead without the joined contact columns, which is
// how a lead appears inside a contact's own response.
func toAPICampaignLeadBase(row dbgen.CampaignLead) gen.CampaignLeadBase {
	return gen.CampaignLeadBase{
		Id:              row.ID,
		CampaignId:      row.CampaignID,
		ContactId:       row.ContactID,
		BusinessId:      nullUUID(row.BusinessID),
		Status:          gen.CampaignLeadStatus(row.Status),
		InstantlyLeadId: row.InstantlyLeadID,
		InstantlyStatus: widenInt32(row.InstantlyStatus),
		InterestStatus:  widenInt32(row.InterestStatus),
		InterestLabel:   row.InterestLabel,
		PushedAt:        utcPtr(row.PushedAt),
		PushAttempts:    int(row.PushAttempts),
		LastPushError:   row.LastPushError,
		LastContactedAt: utcPtr(row.LastContactedAt),
		LastOpenedAt:    utcPtr(row.LastOpenedAt),
		LastClickedAt:   utcPtr(row.LastClickedAt),
		LastRepliedAt:   utcPtr(row.LastRepliedAt),
		OpenCount:       int(row.OpenCount),
		ClickCount:      int(row.ClickCount),
		ReplyCount:      int(row.ReplyCount),
		CreatedAt:       utc(row.CreatedAt),
		UpdatedAt:       utc(row.UpdatedAt),
	}
}

func toAPICampaignLeadDetail(detail campaignsvc.LeadDetail) gen.CampaignLeadDetail {
	row := detail.Lead

	assignments := make([]gen.VariantAssignment, 0, len(detail.Assignments))
	for _, assignment := range detail.Assignments {
		assignments = append(assignments, toAPIVariantAssignment(assignment))
	}
	sends := make([]gen.EmailSend, 0, len(detail.Sends))
	for _, send := range detail.Sends {
		sends = append(sends, toAPIEmailSend(send))
	}
	events := make([]gen.ContactEvent, 0, len(detail.Events))
	for _, event := range detail.Events {
		events = append(events, toAPIContactEvent(event))
	}

	return gen.CampaignLeadDetail{
		Id:              row.ID,
		CampaignId:      row.CampaignID,
		ContactId:       row.ContactID,
		BusinessId:      nullUUID(row.BusinessID),
		Status:          gen.CampaignLeadStatus(row.Status),
		InstantlyLeadId: row.InstantlyLeadID,
		InstantlyStatus: widenInt32(row.InstantlyStatus),
		InterestStatus:  widenInt32(row.InterestStatus),
		InterestLabel:   row.InterestLabel,
		PushedAt:        utcPtr(row.PushedAt),
		PushAttempts:    int(row.PushAttempts),
		LastPushError:   row.LastPushError,
		LastContactedAt: utcPtr(row.LastContactedAt),
		LastOpenedAt:    utcPtr(row.LastOpenedAt),
		LastClickedAt:   utcPtr(row.LastClickedAt),
		LastRepliedAt:   utcPtr(row.LastRepliedAt),
		OpenCount:       int(row.OpenCount),
		ClickCount:      int(row.ClickCount),
		ReplyCount:      int(row.ReplyCount),
		CreatedAt:       utc(row.CreatedAt),
		UpdatedAt:       utc(row.UpdatedAt),
		Email:           row.Email,
		FirstName:       row.FirstName,
		LastName:        row.LastName,
		Company:         row.Company,
		Title:           row.Title,
		LifecycleStage:  gen.ContactStage(row.LifecycleStage),
		SuppressedAt:    utcPtr(row.SuppressedAt),
		VariantNames:    stringList(row.VariantNames),
		SendsTotal:      row.SendsTotal,

		Contact:     toAPIContactFromRow(detail.Contact),
		Assignments: assignments,
		Sends:       sends,
		Events:      events,
	}
}

func toAPIVariantAssignment(row dbgen.VariantAssignment) gen.VariantAssignment {
	return gen.VariantAssignment{
		Id:                 row.ID,
		CampaignLeadId:     row.CampaignLeadID,
		Step:               int(row.Step),
		VariantId:          row.VariantID,
		WeightsVersion:     int(row.WeightsVersion),
		SeedHash:           row.SeedHash,
		RenderedSubject:    row.RenderedSubject,
		RenderedBody:       row.RenderedBody,
		ComponentIds:       uuidList(row.ComponentIds),
		SubjectComponentId: nullUUID(row.SubjectComponentID),
		HookComponentId:    nullUUID(row.HookComponentID),
		CtaComponentId:     nullUUID(row.CtaComponentID),
		AssignedAt:         utc(row.AssignedAt),
		LockedAt:           utcPtr(row.LockedAt),
	}
}

func toAPIEmailSend(row dbgen.EmailSend) gen.EmailSend {
	out := gen.EmailSend{
		Id:                  row.ID,
		CampaignLeadId:      row.CampaignLeadID,
		AssignmentId:        nullUUID(row.AssignmentID),
		CampaignId:          row.CampaignID,
		ContactId:           row.ContactID,
		Step:                int(row.Step),
		VariantId:           nullUUID(row.VariantID),
		SendingAccountEmail: row.SendingAccountEmail,
		SendingAccountId:    nullUUID(row.SendingAccountID),
		InstantlyEmailId:    row.InstantlyEmailID,
		ProviderMessageId:   row.ProviderMessageID,
		SubjectSnapshot:     row.SubjectSnapshot,
		SentAt:              utc(row.SentAt),
		FirstOpenedAt:       utcPtr(row.FirstOpenedAt),
		LastOpenedAt:        utcPtr(row.LastOpenedAt),
		OpenCount:           int(row.OpenCount),
		FirstClickedAt:      utcPtr(row.FirstClickedAt),
		LastClickedAt:       utcPtr(row.LastClickedAt),
		ClickCount:          int(row.ClickCount),
		RepliedAt:           utcPtr(row.RepliedAt),
		BouncedAt:           utcPtr(row.BouncedAt),
		UnsubscribedAt:      utcPtr(row.UnsubscribedAt),
		Source:              gen.SendSource(row.Source),
		CreatedAt:           utc(row.CreatedAt),
		UpdatedAt:           utc(row.UpdatedAt),
	}
	if row.ReplyClassification != nil && *row.ReplyClassification != "" {
		classification := gen.ReplyClassification(*row.ReplyClassification)
		out.ReplyClassification = &classification
	}
	return out
}

func toAPILeadImportResult(in campaignsvc.ImportResult) gen.LeadImportResult {
	return gen.LeadImportResult{
		Matched:           in.Matched,
		Imported:          in.Imported,
		SkippedSuppressed: in.SkippedSuppressed,
		SkippedExisting:   in.SkippedExisting,
		SkippedInvalid:    in.SkippedInvalid,
		Capped:            in.Capped,
	}
}

/* --------------------------------------------------------------- contacts */

func toAPIContact(row db.ContactRow) gen.Contact {
	hasConsent, campaignCount, subscriptionCount := row.HasConsent, row.CampaignCount, row.SubscriptionCount
	out := gen.Contact{
		Id:                row.ID,
		Email:             row.Email,
		Domain:            row.Domain,
		FirstName:         row.FirstName,
		LastName:          row.LastName,
		Company:           row.Company,
		Title:             row.Title,
		Phone:             row.Phone,
		Website:           row.Website,
		BusinessId:        nullUUID(row.BusinessID),
		Source:            gen.ContactSource(row.Source),
		LifecycleStage:    gen.ContactStage(row.LifecycleStage),
		StageChangedAt:    utc(row.StageChangedAt),
		SuppressedAt:      utcPtr(row.SuppressedAt),
		SuppressionReason: toAPISuppressionReason(row.SuppressionReason),
		Attributes:        decodeJSONObject(row.Attributes),
		LastEventAt:       utcPtr(row.LastEventAt),
		CreatedAt:         utc(row.CreatedAt),
		UpdatedAt:         utc(row.UpdatedAt),
		HasConsent:        &hasConsent,
		CampaignCount:     &campaignCount,
		SubscriptionCount: &subscriptionCount,
	}
	return out
}

// toAPIContactFromRow renders a bare contact row: the consent flag and the counts
// are only computed by the list query, so a nested contact omits them.
func toAPIContactFromRow(row dbgen.Contact) gen.Contact {
	return gen.Contact{
		Id:                row.ID,
		Email:             row.Email,
		Domain:            row.Domain,
		FirstName:         row.FirstName,
		LastName:          row.LastName,
		Company:           row.Company,
		Title:             row.Title,
		Phone:             row.Phone,
		Website:           row.Website,
		BusinessId:        nullUUID(row.BusinessID),
		Source:            gen.ContactSource(row.Source),
		LifecycleStage:    gen.ContactStage(row.LifecycleStage),
		StageChangedAt:    utc(row.StageChangedAt),
		SuppressedAt:      utcPtr(row.SuppressedAt),
		SuppressionReason: toAPISuppressionReason(row.SuppressionReason),
		Attributes:        decodeJSONObject(row.Attributes),
		LastEventAt:       utcPtr(row.LastEventAt),
		CreatedAt:         utc(row.CreatedAt),
		UpdatedAt:         utc(row.UpdatedAt),
	}
}

func toAPIContactDetail(detail campaignsvc.ContactDetail) gen.ContactDetail {
	row := detail.Contact
	hasConsent, campaignCount, subscriptionCount := row.HasConsent, row.CampaignCount, row.SubscriptionCount

	leads := make([]gen.ContactCampaignLead, 0, len(detail.Leads))
	for _, lead := range detail.Leads {
		leads = append(leads, gen.ContactCampaignLead{
			Campaign: toAPICampaignSummary(lead.Campaign),
			Lead:     toAPICampaignLeadBase(lead.Lead),
		})
	}
	consents := make([]gen.Consent, 0, len(detail.Consents))
	for _, record := range detail.Consents {
		consents = append(consents, toAPIConsent(record))
	}
	suppressions := make([]gen.Suppression, 0, len(detail.Suppressions))
	for _, record := range detail.Suppressions {
		suppressions = append(suppressions, toAPISuppression(record))
	}
	subscriptions := make([]gen.NewsletterSubscription, 0, len(detail.Subscriptions))
	for _, record := range detail.Subscriptions {
		subscriptions = append(subscriptions, toAPINewsletterSubscription(record))
	}

	return gen.ContactDetail{
		Id:                row.ID,
		Email:             row.Email,
		Domain:            row.Domain,
		FirstName:         row.FirstName,
		LastName:          row.LastName,
		Company:           row.Company,
		Title:             row.Title,
		Phone:             row.Phone,
		Website:           row.Website,
		BusinessId:        nullUUID(row.BusinessID),
		Source:            gen.ContactSource(row.Source),
		LifecycleStage:    gen.ContactStage(row.LifecycleStage),
		StageChangedAt:    utc(row.StageChangedAt),
		SuppressedAt:      utcPtr(row.SuppressedAt),
		SuppressionReason: toAPISuppressionReason(row.SuppressionReason),
		Attributes:        decodeJSONObject(row.Attributes),
		LastEventAt:       utcPtr(row.LastEventAt),
		CreatedAt:         utc(row.CreatedAt),
		UpdatedAt:         utc(row.UpdatedAt),
		HasConsent:        &hasConsent,
		CampaignCount:     &campaignCount,
		SubscriptionCount: &subscriptionCount,

		Leads:         leads,
		Consents:      consents,
		Suppressions:  suppressions,
		Subscriptions: subscriptions,
	}
}

func toAPISuppressionReason(value *string) *gen.SuppressionReason {
	if value == nil || *value == "" {
		return nil
	}
	reason := gen.SuppressionReason(*value)
	return &reason
}

func toAPIConsent(row dbgen.ContactConsent) gen.Consent {
	return gen.Consent{
		Id:              row.ID,
		ContactId:       row.ContactID,
		Source:          gen.ConsentSource(row.Source),
		CapturedAt:      utc(row.CapturedAt),
		Evidence:        row.Evidence,
		CapturedBy:      row.CapturedBy,
		CampaignLeadId:  nullUUID(row.CampaignLeadID),
		ProviderEventId: nullUUID(row.ProviderEventID),
		RevokedAt:       utcPtr(row.RevokedAt),
		RevokeReason:    row.RevokeReason,
		CreatedAt:       utc(row.CreatedAt),
	}
}

func toAPISuppression(row dbgen.ContactSuppression) gen.Suppression {
	return gen.Suppression{
		Id:              row.ID,
		ContactId:       row.ContactID,
		Reason:          gen.SuppressionReason(row.Reason),
		Source:          gen.SuppressionSource(row.Source),
		Note:            row.Note,
		CampaignLeadId:  nullUUID(row.CampaignLeadID),
		ProviderEventId: nullUUID(row.ProviderEventID),
		CreatedAt:       utc(row.CreatedAt),
		LiftedAt:        utcPtr(row.LiftedAt),
		LiftedNote:      row.LiftedNote,
	}
}

func toAPIContactEvent(row dbgen.ContactEvent) gen.ContactEvent {
	return gen.ContactEvent{
		Id:              row.ID,
		ContactId:       row.ContactID,
		CampaignId:      nullUUID(row.CampaignID),
		CampaignLeadId:  nullUUID(row.CampaignLeadID),
		AssignmentId:    nullUUID(row.AssignmentID),
		VariantId:       nullUUID(row.VariantID),
		SendId:          nullUUID(row.SendID),
		Step:            widenInt32(row.Step),
		Type:            gen.ContactEventType(row.Type),
		OccurredAt:      utc(row.OccurredAt),
		Source:          gen.ContactEventSource(row.Source),
		ProviderEventId: nullUUID(row.ProviderEventID),
		StageBefore:     toAPIContactStage(row.StageBefore),
		StageAfter:      toAPIContactStage(row.StageAfter),
		Data:            decodeJSONObject(row.Data),
		CreatedAt:       utc(row.CreatedAt),
	}
}

func toAPIContactStage(value *string) *gen.ContactStage {
	if value == nil || *value == "" {
		return nil
	}
	stage := gen.ContactStage(*value)
	return &stage
}

/* ---------------------------------------------------------------- content */

func toAPIEmailComponent(row dbgen.EmailComponent) gen.EmailComponent {
	return gen.EmailComponent{
		Id:             row.ID,
		Type:           gen.ComponentType(row.Type),
		Name:           row.Name,
		Body:           row.Body,
		Status:         gen.ContentStatus(row.Status),
		Tags:           stringList(row.Tags),
		Language:       row.Language,
		AiGenerationId: nullUUID(row.AiGenerationID),
		Placeholders:   stringList(row.Placeholders),
		CreatedAt:      utc(row.CreatedAt),
		UpdatedAt:      utc(row.UpdatedAt),
		ArchivedAt:     utcPtr(row.ArchivedAt),
	}
}

func toAPIEmailVariant(row dbgen.EmailVariant) gen.EmailVariant {
	return gen.EmailVariant{
		Id:              row.ID,
		Name:            row.Name,
		Step:            int(row.Step),
		Status:          gen.ContentStatus(row.Status),
		SubjectTemplate: row.SubjectTemplate,
		BodyTemplate:    row.BodyTemplate,
		ComponentIds:    uuidList(row.ComponentIds),
		AiGenerationId:  nullUUID(row.AiGenerationID),
		Notes:           row.Notes,
		CreatedAt:       utc(row.CreatedAt),
		UpdatedAt:       utc(row.UpdatedAt),
		ArchivedAt:      utcPtr(row.ArchivedAt),
	}
}

// toAPIEmailVariantComponent renders one slot of a variant: the component itself,
// plus where it sits in the assembly.
func toAPIEmailVariantComponent(row dbgen.ListVariantComponentsRow) gen.EmailVariantComponent {
	return gen.EmailVariantComponent{
		Id:             row.ID,
		Type:           gen.ComponentType(row.Type),
		Name:           row.Name,
		Body:           row.Body,
		Status:         gen.ContentStatus(row.Status),
		Tags:           stringList(row.Tags),
		Language:       row.Language,
		AiGenerationId: nullUUID(row.AiGenerationID),
		Placeholders:   stringList(row.Placeholders),
		CreatedAt:      utc(row.CreatedAt),
		UpdatedAt:      utc(row.UpdatedAt),
		ArchivedAt:     utcPtr(row.ArchivedAt),
		Position:       int(row.Position),
		Slot:           gen.ComponentType(row.Slot),
	}
}

func toAPIEmailVariantDetail(detail campaignsvc.VariantDetail) gen.EmailVariantDetail {
	row := detail.Variant

	components := make([]gen.EmailVariantComponent, 0, len(detail.Components))
	for _, component := range detail.Components {
		components = append(components, toAPIEmailVariantComponent(component))
	}
	campaigns := make([]gen.CampaignSummary, 0, len(detail.Campaigns))
	for _, camp := range detail.Campaigns {
		campaigns = append(campaigns, toAPICampaignSummary(camp))
	}

	return gen.EmailVariantDetail{
		Id:              row.ID,
		Name:            row.Name,
		Step:            int(row.Step),
		Status:          gen.ContentStatus(row.Status),
		SubjectTemplate: row.SubjectTemplate,
		BodyTemplate:    row.BodyTemplate,
		ComponentIds:    uuidList(row.ComponentIds),
		AiGenerationId:  nullUUID(row.AiGenerationID),
		Notes:           row.Notes,
		CreatedAt:       utc(row.CreatedAt),
		UpdatedAt:       utc(row.UpdatedAt),
		ArchivedAt:      utcPtr(row.ArchivedAt),
		Components:      components,
		Campaigns:       campaigns,
	}
}

func toAPIComponentUsage(in campaignsvc.ComponentUsage) gen.ComponentUsage {
	variants := make([]gen.EmailVariant, 0, len(in.Variants))
	for _, variant := range in.Variants {
		variants = append(variants, toAPIEmailVariant(variant))
	}
	campaigns := make([]gen.CampaignSummary, 0, len(in.Campaigns))
	for _, camp := range in.Campaigns {
		campaigns = append(campaigns, toAPICampaignSummary(camp))
	}
	return gen.ComponentUsage{
		Variants:          variants,
		Campaigns:         campaigns,
		LockedAssignments: in.LockedAssignments,
	}
}

func toAPIPreview(in campaignsvc.Preview) gen.EmailPreview {
	return gen.EmailPreview{
		Subject:      in.Subject,
		BodyText:     in.BodyText,
		BodyHtml:     in.BodyHTML,
		Placeholders: stringList(in.Placeholders),
	}
}

/* ------------------------------------------------------- sending accounts */

func toAPISendingAccount(row dbgen.SendingAccount) gen.SendingAccount {
	return gen.SendingAccount{
		Id:                   row.ID,
		Email:                row.Email,
		FirstName:            row.FirstName,
		LastName:             row.LastName,
		ProviderCode:         widenInt32(row.ProviderCode),
		Status:               int(row.Status),
		WarmupStatus:         widenInt32(row.WarmupStatus),
		DailyLimit:           widenInt32(row.DailyLimit),
		SendingGap:           widenInt32(row.SendingGap),
		WarmupScore:          widenInt32(row.WarmupScore),
		StatusMessage:        row.StatusMessage,
		TrackingDomain:       row.TrackingDomain,
		TrackingDomainStatus: row.TrackingDomainStatus,
		SetupPending:         row.SetupPending,
		IsManaged:            row.IsManaged,
		LastSyncedAt:         utcPtr(row.LastSyncedAt),
		CreatedAt:            utc(row.CreatedAt),
		UpdatedAt:            utc(row.UpdatedAt),
	}
}

// toAPISendingAccountView is toAPISendingAccount plus the stat fields, which are
// only present when the account is read through the sending-accounts endpoints.
func toAPISendingAccountView(view campaignsvc.SendingAccountView) gen.SendingAccount {
	out := toAPISendingAccount(view.Account)
	sent, bounced, replies := view.Sent30d, view.Bounced30d, view.Replies30d
	campaigns := view.Campaigns
	localSends, localBounces, localReplies := view.LocalSends, view.LocalBounces, view.LocalReplies
	out.Sent30d = &sent
	out.Bounced30d = &bounced
	out.Replies30d = &replies
	out.Campaigns = &campaigns
	out.LocalSends = &localSends
	out.LocalBounces = &localBounces
	out.LocalReplies = &localReplies
	out.LastActivityAt = utcPtr(view.LastActivity)
	return out
}

func toAPISendingAccountDaily(row dbgen.SendingAccountStatsDaily) gen.SendingAccountDaily {
	return gen.SendingAccountDaily{
		SendingAccountId:  row.SendingAccountID,
		Day:               openapi_types.Date{Time: row.Day.Time},
		Sent:              int(row.Sent),
		Bounced:           int(row.Bounced),
		Contacted:         int(row.Contacted),
		NewLeadsContacted: int(row.NewLeadsContacted),
		Opened:            int(row.Opened),
		UniqueOpened:      int(row.UniqueOpened),
		Replies:           int(row.Replies),
		UniqueReplies:     int(row.UniqueReplies),
		Clicks:            int(row.Clicks),
		UniqueClicks:      int(row.UniqueClicks),
		FetchedAt:         utc(row.FetchedAt),
	}
}

/* ------------------------------------------------------------- newsletter */

func toAPINewsletterAudience(row dbgen.NewsletterAudience) gen.NewsletterAudience {
	return gen.NewsletterAudience{
		Id:               row.ID,
		MailchimpListId:  row.MailchimpListID,
		Name:             row.Name,
		DoubleOptin:      row.DoubleOptin,
		AllowSingleOptIn: row.AllowSingleOptIn,
		DefaultTags:      stringList(row.DefaultTags),
		WebhookId:        row.WebhookID,
		WebhookUrl:       row.WebhookUrl,
		MemberCount:      widenInt32(row.MemberCount),
		Stats:            decodeJSONObject(row.Stats),
		IsDefault:        row.IsDefault,
		LastSyncedAt:     utcPtr(row.LastSyncedAt),
		LastSyncError:    row.LastSyncError,
		CreatedAt:        utc(row.CreatedAt),
		UpdatedAt:        utc(row.UpdatedAt),
	}
}

// toAPINewsletterSubscription renders one subscription. The list query returns a
// wider row, which the newsletter handler narrows to this one before mapping, so
// there is a single place where a subscription's shape is decided.
func toAPINewsletterSubscription(row dbgen.NewsletterSubscription) gen.NewsletterSubscription {
	return gen.NewsletterSubscription{
		Id:                 row.ID,
		ContactId:          row.ContactID,
		AudienceId:         row.AudienceID,
		ConsentId:          nullUUID(row.ConsentID),
		RequestedStatus:    gen.RequestedSubscriptionStatus(row.RequestedStatus),
		Status:             gen.SubscriptionStatus(row.Status),
		SyncStatus:         gen.SyncStatus(row.SyncStatus),
		SubscriberHash:     row.SubscriberHash,
		UniqueEmailId:      row.UniqueEmailID,
		MailchimpContactId: row.MailchimpContactID,
		WebId:              row.WebID,
		ClaimedAt:          utcPtr(row.ClaimedAt),
		PushedAt:           utcPtr(row.PushedAt),
		LastSyncedAt:       utcPtr(row.LastSyncedAt),
		LastError:          row.LastError,
		SyncAttempts:       int(row.SyncAttempts),
		SubscribedAt:       utcPtr(row.SubscribedAt),
		UnsubscribedAt:     utcPtr(row.UnsubscribedAt),
		UnsubscribeReason:  row.UnsubscribeReason,
		Tags:               stringList(row.Tags),
		CreatedAt:          utc(row.CreatedAt),
		UpdatedAt:          utc(row.UpdatedAt),
	}
}

func toAPIEligibleContact(in campaignsvc.EligibleContact) gen.EligibleContact {
	return gen.EligibleContact{
		Contact:  toAPIEligibleContactRecord(in.Contact),
		Decision: toAPIConsentDecision(in.Decision),
	}
}

func toAPIEligibleContactRecord(row dbgen.ListNewsletterEligibleContactsRow) gen.EligibleContactRecord {
	out := gen.EligibleContactRecord{
		Id:                row.ID,
		Email:             row.Email,
		Domain:            row.Domain,
		FirstName:         row.FirstName,
		LastName:          row.LastName,
		Company:           row.Company,
		Title:             row.Title,
		Phone:             row.Phone,
		Website:           row.Website,
		BusinessId:        nullUUID(row.BusinessID),
		Source:            gen.ContactSource(row.Source),
		LifecycleStage:    gen.ContactStage(row.LifecycleStage),
		StageChangedAt:    utc(row.StageChangedAt),
		SuppressedAt:      utcPtr(row.SuppressedAt),
		SuppressionReason: toAPISuppressionReason(row.SuppressionReason),
		Attributes:        decodeJSONObject(row.Attributes),
		LastEventAt:       utcPtr(row.LastEventAt),
		CreatedAt:         utc(row.CreatedAt),
		UpdatedAt:         utc(row.UpdatedAt),
		ConsentId:         nullUUID(row.ConsentID),
		ConsentCapturedAt: utcPtr(row.ConsentCapturedAt),
	}
	if row.ConsentSource != nil && *row.ConsentSource != "" {
		source := gen.ConsentSource(*row.ConsentSource)
		out.ConsentSource = &source
	}
	return out
}

func toAPIConsentDecision(in consent.Decision) gen.ConsentDecision {
	return gen.ConsentDecision{
		Eligible:        in.Eligible,
		RequestedStatus: gen.RequestedSubscriptionStatus(in.RequestedStatus),
		Reason:          in.Reason,
		Explanation:     in.Explanation,
	}
}

func toAPINewsletterPushResult(in campaignsvc.PushResult) gen.NewsletterPushResult {
	queued := make([]openapi_types.UUID, 0, len(in.Queued))
	queued = append(queued, in.Queued...)
	rejected := make([]gen.NewsletterPushRejection, 0, len(in.Rejected))
	for _, item := range in.Rejected {
		rejected = append(rejected, gen.NewsletterPushRejection{ContactId: item.ContactID, Reason: item.Reason})
	}
	return gen.NewsletterPushResult{Queued: queued, Rejected: rejected}
}

func toAPINewsletterStats(in campaignsvc.NewsletterStats) gen.NewsletterStats {
	return gen.NewsletterStats{
		ByStatus:       counts(in.ByStatus),
		Eligible:       in.Eligible,
		Subscribed:     in.Subscribed,
		Pending:        in.Pending,
		ConversionRate: in.ConversionRate,
	}
}

/* ----------------------------------------------------------- integrations */

func toAPIProviderEvent(row dbgen.ProviderEvent) gen.ProviderEvent {
	return gen.ProviderEvent{
		Id:             row.ID,
		Provider:       gen.ProviderName(row.Provider),
		EventType:      row.EventType,
		DedupeKey:      row.DedupeKey,
		ReceivedAt:     utc(row.ReceivedAt),
		OccurredAt:     utcPtr(row.OccurredAt),
		Raw:            decodeJSONObject(row.Raw),
		Source:         gen.ProviderEventSource(row.Source),
		CampaignId:     nullUUID(row.CampaignID),
		ContactId:      nullUUID(row.ContactID),
		CampaignLeadId: nullUUID(row.CampaignLeadID),
		ProcessedAt:    utcPtr(row.ProcessedAt),
		Attempts:       int(row.Attempts),
		Error:          row.Error,
	}
}

func toAPISyncRun(row dbgen.SyncRun) gen.SyncRun {
	return gen.SyncRun{
		Id:           row.ID,
		Kind:         gen.SyncRunKind(row.Kind),
		Status:       gen.SyncRunStatus(row.Status),
		TargetId:     nullUUID(row.TargetID),
		StartedAt:    utc(row.StartedAt),
		FinishedAt:   utcPtr(row.FinishedAt),
		ItemsSeen:    int(row.ItemsSeen),
		ItemsUpdated: int(row.ItemsUpdated),
		Error:        row.Error,
		Details:      decodeJSONObject(row.Details),
	}
}

func toAPIWebhookStatus(in campaignsvc.WebhookStatus) gen.WebhookStatus {
	return gen.WebhookStatus{
		Registered: in.Registered,
		Url:        in.URL,
		ProviderId: in.ProviderID,
		Status:     widenInt32(in.Status),
		Error:      in.Error,
	}
}

func toAPIEventSummary(in campaignsvc.EventSummary) gen.EventSummary {
	return gen.EventSummary{
		Total:          in.Total,
		Unprocessed:    in.Unprocessed,
		Errored:        in.Errored,
		LastReceivedAt: utcPtr(in.LastReceivedAt),
	}
}

func toAPIProviderStatus(in campaignsvc.ProviderStatus) gen.ProviderStatus {
	out := gen.ProviderStatus{
		SourceId:     in.SourceID,
		HasKey:       in.HasKey,
		Enabled:      in.Enabled,
		LastTestedAt: utcPtr(in.LastTestedAt),
		LastTestOk:   in.LastTestOK,
		Webhook:      toAPIWebhookStatus(in.Webhook),
		Events:       toAPIEventSummary(in.Events),
	}
	if in.LastSync != nil {
		run := toAPISyncRun(*in.LastSync)
		out.LastSync = &run
	}
	return out
}

func toAPIIntegrations(in campaignsvc.IntegrationsStatus) gen.Integrations {
	return gen.Integrations{
		PublicBaseUrl: in.PublicBaseURL,
		Instantly:     toAPIProviderStatus(in.Instantly),
		Mailchimp:     toAPIProviderStatus(in.Mailchimp),
		Audiences:     in.Audiences,
		Ai:            toAPIAIProvider(in.AI),
	}
}

func toAPIProviderTestResult(in campaignsvc.TestResult) gen.ProviderTestResult {
	out := gen.ProviderTestResult{Ok: in.OK, Detail: in.Detail}
	if in.AccountsCount > 0 {
		count := in.AccountsCount
		out.AccountsCount = &count
	}
	return out
}

/* -------------------------------------------------------------------- AI */

func toAPIAIProvider(in campaignsvc.AIProviderInfo) gen.AIProvider {
	return gen.AIProvider{
		Provider: gen.AIProviderName(in.Provider),
		Mode:     gen.AIMode(in.Mode),
		Model:    optString(in.Model),
		Label:    in.Label,
	}
}

func toAPIAIBrief(in ai.Brief) gen.AIBrief {
	out := gen.AIBrief{
		CampaignGoal:      optString(in.CampaignGoal),
		Company:           optString(in.Company),
		Product:           optString(in.Product),
		TargetIndustry:    optString(in.TargetIndustry),
		TargetJobTitle:    optString(in.TargetJobTitle),
		TargetCompanySize: optString(in.TargetCompanySize),
		ValueProposition:  optString(in.ValueProposition),
		DesiredCta:        optString(in.DesiredCTA),
		Tone:              optString(in.Tone),
		Language:          optString(in.Language),
		AdditionalContext: optString(in.AdditionalContext),
		SubjectCount:      optInt(in.SubjectCount),
		HookCount:         optInt(in.HookCount),
		BodyCount:         optInt(in.BodyCount),
		CtaCount:          optInt(in.CTACount),
		VariantCount:      optInt(in.VariantCount),
	}
	if len(in.PainPoints) > 0 {
		points := in.PainPoints
		out.PainPoints = &points
	}
	return out
}

func toAPIAIOutput(in ai.Output) gen.AIOutput {
	components := make([]gen.AIOutputComponent, 0, len(in.Components))
	for _, component := range in.Components {
		item := gen.AIOutputComponent{
			Type: gen.ComponentType(component.Type),
			Name: component.Name,
			Body: component.Body,
		}
		if len(component.Tags) > 0 {
			tags := component.Tags
			item.Tags = &tags
		}
		components = append(components, item)
	}
	variants := make([]gen.AIOutputVariant, 0, len(in.Variants))
	for _, variant := range in.Variants {
		variants = append(variants, gen.AIOutputVariant{
			Name:      variant.Name,
			Subject:   variant.SubjectIndex,
			Hook:      variant.HookIndex,
			Problem:   variant.ProblemIndex,
			ValueProp: variant.ValuePropIndex,
			Proof:     variant.ProofIndex,
			Cta:       variant.CTAIndex,
			Closing:   variant.ClosingIndex,
			Ps:        variant.PSIndex,
		})
	}
	out := gen.AIOutput{Components: components, Variants: variants}
	if len(in.Warnings) > 0 {
		warnings := in.Warnings
		out.Warnings = &warnings
	}
	return out
}

func toAPIAIGeneration(view campaignsvc.GenerationView) gen.AIGeneration {
	row := view.Generation
	out := gen.AIGeneration{
		Id:             row.ID,
		Provider:       gen.AIProviderName(row.Provider),
		Model:          row.Model,
		Status:         gen.GenerationStatus(row.Status),
		CampaignId:     nullUUID(row.CampaignID),
		Brief:          toAPIAIBrief(view.Brief),
		Prompt:         row.Prompt,
		PromptVersion:  row.PromptVersion,
		ComponentCount: int(row.ComponentCount),
		VariantCount:   int(row.VariantCount),
		Error:          row.Error,
		CreatedAt:      utc(row.CreatedAt),
		ParsedAt:       utcPtr(row.ParsedAt),
		ImportedAt:     utcPtr(row.ImportedAt),
	}
	if view.Output != nil {
		parsed := toAPIAIOutput(*view.Output)
		out.Parsed = &parsed
	}
	return out
}

func toAPIAIImportResult(in campaignsvc.ImportOutcome) gen.AIImportResult {
	components := make([]gen.EmailComponent, 0, len(in.Components))
	for _, component := range in.Components {
		components = append(components, toAPIEmailComponent(component))
	}
	variants := make([]gen.EmailVariant, 0, len(in.Variants))
	for _, variant := range in.Variants {
		variants = append(variants, toAPIEmailVariant(variant))
	}
	return gen.AIImportResult{Components: components, Variants: variants}
}

/* -------------------------------------------------------------- analytics */

func toAPIMetrics(in campaignsvc.Metrics) gen.Metrics {
	return gen.Metrics{
		Sends:             in.Sends,
		UniqueContacts:    in.UniqueContacts,
		Opened:            in.Opened,
		Clicked:           in.Clicked,
		Replied:           in.Replied,
		PositiveReplies:   in.PositiveReplies,
		Bounced:           in.Bounced,
		Unsubscribed:      in.Unsubscribed,
		OpenRate:          in.OpenRate,
		ClickRate:         in.ClickRate,
		ReplyRate:         in.ReplyRate,
		PositiveReplyRate: in.PositiveRate,
		BounceRate:        in.BounceRate,
		UnsubscribeRate:   in.UnsubscribeRate,
	}
}

func toAPIFunnelStep(in campaignsvc.FunnelStep) gen.FunnelStep {
	return gen.FunnelStep{
		Stage:       gen.ContactStage(in.Stage),
		Label:       in.Label,
		Current:     in.Current,
		EverReached: in.EverReached,
	}
}

func toAPIFunnel(in []campaignsvc.FunnelStep) []gen.FunnelStep {
	out := make([]gen.FunnelStep, 0, len(in))
	for _, step := range in {
		out = append(out, toAPIFunnelStep(step))
	}
	return out
}

func toAPIOverview(in campaignsvc.Overview) gen.CampaignOverview {
	return gen.CampaignOverview{
		Campaigns:          counts(in.Campaigns),
		ContactsByStage:    counts(in.ContactsByStage),
		Local:              toAPIMetrics(in.Local),
		Interested:         in.Interested,
		NewsletterEligible: in.NewsletterEligible,
		Subscribers:        in.Subscribers,
		ConversionRate:     in.ConversionRate,
		Funnel:             toAPIFunnel(in.Funnel),
	}
}

func toAPICampaignAnalytics(in campaignsvc.CampaignAnalytics) gen.CampaignAnalytics {
	mismatch := make([]gen.Mismatch, 0, len(in.Mismatch))
	for _, item := range in.Mismatch {
		mismatch = append(mismatch, gen.Mismatch{Metric: item.Metric, Local: item.Local, Instantly: item.Instantly})
	}
	daily := make([]gen.DailyPoint, 0, len(in.Daily))
	for _, point := range in.Daily {
		daily = append(daily, gen.DailyPoint{
			Day:     apiDate(point.Day),
			Sends:   point.Sends,
			Opened:  point.Opened,
			Clicked: point.Clicked,
			Replied: point.Replied,
			Bounced: point.Bounced,
		})
	}

	out := gen.CampaignAnalytics{
		Local:          toAPIMetrics(in.Local),
		Mismatch:       mismatch,
		Daily:          daily,
		Funnel:         toAPIFunnel(in.Funnel),
		Interested:     in.Interested,
		Eligible:       in.Eligible,
		Subscribers:    in.Subscribers,
		ConversionRate: in.ConversionRate,
		LeadCounts:     counts(in.LeadCounts),
	}
	if len(in.Instantly) > 0 {
		instantly := in.Instantly
		out.Instantly = &instantly
	}
	return out
}

func toAPIVariantAnalytics(in campaignsvc.VariantAnalytics) gen.VariantAnalytics {
	metrics := toAPIMetrics(in.Metrics)
	return gen.VariantAnalytics{
		VariantId:         in.VariantID,
		VariantName:       in.VariantName,
		Step:              in.Step,
		Assigned:          in.Assigned,
		Sends:             metrics.Sends,
		UniqueContacts:    metrics.UniqueContacts,
		Opened:            metrics.Opened,
		Clicked:           metrics.Clicked,
		Replied:           metrics.Replied,
		PositiveReplies:   metrics.PositiveReplies,
		Bounced:           metrics.Bounced,
		Unsubscribed:      metrics.Unsubscribed,
		OpenRate:          metrics.OpenRate,
		ClickRate:         metrics.ClickRate,
		ReplyRate:         metrics.ReplyRate,
		PositiveReplyRate: metrics.PositiveReplyRate,
		BounceRate:        metrics.BounceRate,
		UnsubscribeRate:   metrics.UnsubscribeRate,
	}
}

func toAPIComponentAnalytics(in campaignsvc.ComponentAnalytics) gen.ComponentAnalytics {
	metrics := toAPIMetrics(in.Metrics)
	return gen.ComponentAnalytics{
		ComponentId:       in.ComponentID,
		Type:              gen.ComponentType(in.Type),
		Name:              in.Name,
		Sends:             metrics.Sends,
		UniqueContacts:    metrics.UniqueContacts,
		Opened:            metrics.Opened,
		Clicked:           metrics.Clicked,
		Replied:           metrics.Replied,
		PositiveReplies:   metrics.PositiveReplies,
		Bounced:           metrics.Bounced,
		Unsubscribed:      metrics.Unsubscribed,
		OpenRate:          metrics.OpenRate,
		ClickRate:         metrics.ClickRate,
		ReplyRate:         metrics.ReplyRate,
		PositiveReplyRate: metrics.PositiveReplyRate,
		BounceRate:        metrics.BounceRate,
		UnsubscribeRate:   metrics.UnsubscribeRate,
	}
}

func toAPIAccountAnalytics(in campaignsvc.AccountAnalytics) gen.AccountAnalytics {
	return gen.AccountAnalytics{
		SendingAccountId:    nullUUID(in.SendingAccountID),
		Email:               in.Email,
		Local:               toAPIMetrics(in.Local),
		InstantlySent:       in.InstantlySent,
		InstantlyBounced:    in.InstantlyBounced,
		InstantlyReplies:    in.InstantlyReplies,
		InstantlyBounceRate: in.InstantlyBounceRate,
	}
}
