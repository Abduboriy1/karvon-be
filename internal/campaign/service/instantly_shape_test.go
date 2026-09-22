package service_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// The Instantly campaign we create is a placeholder: one variant per step whose
// subject and body are the custom variables each lead's push fills in. Everything
// about weighted distribution and per-component analytics depends on that shape,
// so it is pinned here.
func TestTheInstantlySequenceIsOneVariantPerStepOfCustomVariables(t *testing.T) {
	sequences := service.SequencesFor(3, []int{2, 4, 0})
	if len(sequences) != 1 {
		t.Fatalf("got %d sequences, want 1 (Instantly only reads the first)", len(sequences))
	}
	steps := sequences[0].Steps
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(steps))
	}
	for i, step := range steps {
		if step.Type != "email" {
			t.Errorf("step %d is type %q, want email", i+1, step.Type)
		}
		if len(step.Variants) != 1 {
			t.Fatalf("step %d has %d variants, want exactly 1", i+1, len(step.Variants))
		}
		v := step.Variants[0]
		wantSubject := "{{" + campaign.SubjectVar(i+1) + "}}"
		wantBody := "{{" + campaign.BodyVar(i+1) + "}}"
		if v.Subject != wantSubject || v.Body != wantBody {
			t.Errorf("step %d carries %q / %q, want %q / %q", i+1, v.Subject, v.Body, wantSubject, wantBody)
		}
	}
	if steps[0].Delay != 2 || steps[1].Delay != 4 {
		t.Errorf("the step delays are %d / %d, want 2 / 4", steps[0].Delay, steps[1].Delay)
	}
	if steps[0].DelayUnit != "days" {
		t.Errorf("the delay unit is %q, want days", steps[0].DelayUnit)
	}
}

func TestStepDelaysAreFilledInWhenTheCampaignDoesNotSayS(t *testing.T) {
	delays := service.StepDelaysFor([]byte(`[5]`), 3)
	if len(delays) != 3 {
		t.Fatalf("got %d delays for 3 steps", len(delays))
	}
	if delays[0] != 5 {
		t.Errorf("the stored delay was lost: %v", delays)
	}
	if delays[1] == 0 || delays[2] == 0 {
		t.Errorf("a follow-up would be sent immediately: %v", delays)
	}
	if got := service.StepDelaysFor(nil, 2); len(got) != 2 {
		t.Errorf("an empty column gave %v", got)
	}
}

func TestTheDefaultScheduleIsUsedWhenTheCampaignHasNone(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("{}"), []byte("null")} {
		sched, err := service.ScheduleFor(raw)
		if err != nil {
			t.Fatalf("an empty schedule was rejected: %v", err)
		}
		if len(sched.Schedules) == 0 {
			t.Fatal("the default schedule has no sending window")
		}
		if sched.Schedules[0].Timezone == "" {
			t.Error("the default window has no timezone; Instantly requires one")
		}
	}
}

func TestAnIncompleteScheduleIsRejectedBeforeItReachesInstantly(t *testing.T) {
	cases := map[string]string{
		"no timezone": `{"schedules":[{"name":"w","timing":{"from":"09:00","to":"17:00"},"days":{"1":true}}]}`,
		"no days":     `{"schedules":[{"name":"w","timing":{"from":"09:00","to":"17:00"},"days":{},"timezone":"Europe/London"}]}`,
		"no from":     `{"schedules":[{"name":"w","timing":{"to":"17:00"},"days":{"1":true},"timezone":"Europe/London"}]}`,
		"not json":    `{"schedules":`,
	}
	for name, raw := range cases {
		if _, err := service.ScheduleFor([]byte(raw)); err == nil {
			t.Errorf("a schedule with %s was accepted", name)
		}
	}
}

func TestTheCampaignSettingsCarryOurSafeDefaults(t *testing.T) {
	settings := service.SettingsFor([]byte(`{"daily_limit":40,"variant_mode":"local"}`))
	if settings["daily_limit"] != float64(40) {
		t.Errorf("the stored daily limit was lost: %v", settings["daily_limit"])
	}
	if _, ok := settings["variant_mode"]; ok {
		t.Error("our own variant_mode key was sent to Instantly")
	}
	if settings["insert_unsubscribe_header"] != true {
		t.Error("the unsubscribe header is not on by default")
	}
	if settings["stop_on_reply"] != true {
		t.Error("stop_on_reply is not on by default")
	}
	// An explicit choice is never overridden.
	settings = service.SettingsFor([]byte(`{"stop_on_reply":false}`))
	if settings["stop_on_reply"] != false {
		t.Error("an explicit stop_on_reply=false was overwritten")
	}
}

func TestTheCreateInputCarriesTheSendingAccountsAndTheSequence(t *testing.T) {
	camp := dbgen.Campaign{
		ID: ids.New(), Name: "Austin gyms", Steps: 2,
		Schedule: []byte(`{}`), Settings: []byte(`{"daily_limit":25}`), StepDelays: []byte(`[3,3]`),
	}
	accounts := []dbgen.SendingAccount{
		{ID: ids.New(), Email: "one@karvon.test"},
		{ID: ids.New(), Email: "two@karvon.test"},
	}
	in, err := service.CreateInputFor(camp, accounts)
	if err != nil {
		t.Fatalf("the campaign could not be shaped: %v", err)
	}
	if in.Name != camp.Name {
		t.Errorf("the name is %q", in.Name)
	}
	if len(in.EmailList) != 2 || in.EmailList[0] != "one@karvon.test" {
		t.Errorf("the sending accounts are %v", in.EmailList)
	}
	if len(in.Sequences[0].Steps) != 2 {
		t.Errorf("the sequence has %d steps, want 2", len(in.Sequences[0].Steps))
	}
	if in.Settings["daily_limit"] != float64(25) {
		t.Errorf("the settings did not survive: %v", in.Settings)
	}

	if _, err := service.CreateInputFor(camp, nil); err == nil {
		t.Error("a campaign with no sending account was shaped anyway")
	}
}

func TestCustomVarsCarryOneRenderedPairPerStep(t *testing.T) {
	assignments := []dbgen.VariantAssignment{
		{Step: 1, RenderedSubject: "Subject one", RenderedBody: "<p>Body one</p>"},
		{Step: 2, RenderedSubject: "Subject two", RenderedBody: "<p>Body two</p>"},
	}
	vars := service.CustomVars(assignments)
	if len(vars) != 4 {
		t.Fatalf("got %d variables for 2 steps, want 4", len(vars))
	}
	if vars[campaign.SubjectVar(1)] != "Subject one" || vars[campaign.BodyVar(2)] != "<p>Body two</p>" {
		t.Fatalf("the variables do not match the assignments: %v", vars)
	}

	raw := service.EncodeCustomVars(vars)
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("the encoded variables are not valid JSON: %v", err)
	}
	if round[campaign.SubjectVar(1)] != "Subject one" {
		t.Error("the encoded variables lost a value")
	}
	if strings.Contains(string(raw), "{{") {
		t.Error("an unrendered placeholder reached the custom variables")
	}
}
