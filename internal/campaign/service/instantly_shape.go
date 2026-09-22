package service

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// The shape of the Instantly campaign we create. One step per local step, each
// with a single variant whose subject and body are the custom variables the push
// job fills per lead. Instantly therefore sees one "variant" and rotates nothing;
// every rotation decision is ours and is recorded on variant_assignments.

// DefaultSchedule is used when a campaign has no schedule of its own: weekdays,
// nine to five, in the account's most common timezone.
var DefaultSchedule = instantly.Schedule{
	Schedules: []instantly.ScheduleWindow{{
		Name:     "Weekdays",
		Timing:   map[string]any{"from": "09:00", "to": "17:00"},
		Days:     map[string]bool{"1": true, "2": true, "3": true, "4": true, "5": true},
		Timezone: "America/New_York",
	}},
}

// ScheduleFor decodes a campaign's schedule column, falling back to the default.
func ScheduleFor(raw []byte) (instantly.Schedule, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return DefaultSchedule, nil
	}
	var sched instantly.Schedule
	if err := json.Unmarshal(raw, &sched); err != nil {
		return instantly.Schedule{}, fmt.Errorf("schedule is not valid JSON: %w", err)
	}
	if len(sched.Schedules) == 0 {
		return DefaultSchedule, nil
	}
	for i, w := range sched.Schedules {
		if w.Timezone == "" {
			return instantly.Schedule{}, fmt.Errorf("schedules[%d].timezone is required", i)
		}
		if len(w.Days) == 0 {
			return instantly.Schedule{}, fmt.Errorf("schedules[%d].days must name at least one day", i)
		}
		if _, ok := w.Timing["from"]; !ok {
			return instantly.Schedule{}, fmt.Errorf("schedules[%d].timing.from is required", i)
		}
		if _, ok := w.Timing["to"]; !ok {
			return instantly.Schedule{}, fmt.Errorf("schedules[%d].timing.to is required", i)
		}
	}
	return sched, nil
}

// SettingsFor decodes the Instantly flags stored on a campaign, dropping our own keys.
func SettingsFor(raw []byte) map[string]any {
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	delete(out, "variant_mode")
	if _, ok := out["insert_unsubscribe_header"]; !ok {
		out["insert_unsubscribe_header"] = true
	}
	if _, ok := out["stop_on_reply"]; !ok {
		out["stop_on_reply"] = true
	}
	return out
}

// StepDelaysFor decodes the per-step delays (days before the NEXT step).
func StepDelaysFor(raw []byte, steps int) []int {
	var delays []int
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &delays)
	}
	for len(delays) < steps {
		delays = append(delays, 3)
	}
	return delays[:steps]
}

// SequencesFor builds the placeholder sequence.
func SequencesFor(steps int, delays []int) []instantly.Sequence {
	out := make([]instantly.Step, 0, steps)
	for step := 1; step <= steps; step++ {
		delay := 0
		if step-1 < len(delays) {
			delay = delays[step-1]
		}
		out = append(out, instantly.Step{
			Type: "email", Delay: delay, DelayUnit: "days",
			Variants: []instantly.Variant{{
				Subject: "{{" + campaign.SubjectVar(step) + "}}",
				Body:    "{{" + campaign.BodyVar(step) + "}}",
			}},
		})
	}
	return []instantly.Sequence{{Steps: out}}
}

// CreateInputFor builds the POST /campaigns body for a local campaign.
func CreateInputFor(camp dbgen.Campaign, accounts []dbgen.SendingAccount) (instantly.CreateCampaignInput, error) {
	sched, err := ScheduleFor(camp.Schedule)
	if err != nil {
		return instantly.CreateCampaignInput{}, err
	}
	if len(accounts) == 0 {
		return instantly.CreateCampaignInput{}, errors.New("no sending accounts are attached")
	}
	emails := make([]string, 0, len(accounts))
	for _, a := range accounts {
		emails = append(emails, a.Email)
	}
	steps := int(camp.Steps)
	return instantly.CreateCampaignInput{
		Name:      camp.Name,
		Schedule:  sched,
		Sequences: SequencesFor(steps, StepDelaysFor(camp.StepDelays, steps)),
		EmailList: emails,
		Settings:  SettingsFor(camp.Settings),
	}, nil
}

// UpdateInputFor builds a PATCH body carrying the name, schedule and flags.
func UpdateInputFor(camp dbgen.Campaign) instantly.UpdateCampaignInput {
	in := instantly.UpdateCampaignInput{Name: &camp.Name, Settings: SettingsFor(camp.Settings)}
	if sched, err := ScheduleFor(camp.Schedule); err == nil {
		in.Schedule = &sched
	}
	return in
}

// UpdateCampaignInputEmailList builds a PATCH body that only changes the accounts.
func UpdateCampaignInputEmailList(emails []string) instantly.UpdateCampaignInput {
	return instantly.UpdateCampaignInput{EmailList: emails}
}
