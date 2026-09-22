package verify

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
	KindRun        = "verify_run"
	KindSelf       = "verify_self"
	KindThirdParty = "verify_third_party"
	KindFinalize   = "verify_run_finalize"
)

// RunArgs expands a run's filter into items and fans out one job per address.
type RunArgs struct {
	RunID uuid.UUID `json:"run_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (RunArgs) Kind() string { return KindRun }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a RunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDefault,
		MaxAttempts: 3,
		Metadata:    RunMetadata(a.RunID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// SelfArgs scores one address locally.
type SelfArgs struct {
	RunID          uuid.UUID `json:"run_id" river:"unique"`
	VerificationID uuid.UUID `json:"verification_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (SelfArgs) Kind() string { return KindSelf }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a SelfArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueVerifySelf,
		MaxAttempts: 3,
		Metadata:    RunMetadata(a.RunID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// ThirdPartyArgs sends one address to the paid provider.
//
// This is the only job in the system that can spend money, so it carries the
// smallest possible payload and relies on the worker to re-check the gate and the
// cache immediately before calling out.
type ThirdPartyArgs struct {
	RunID          uuid.UUID `json:"run_id" river:"unique"`
	VerificationID uuid.UUID `json:"verification_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ThirdPartyArgs) Kind() string { return KindThirdParty }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a ThirdPartyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueVerifyThird,
		MaxAttempts: 5,
		Metadata:    RunMetadata(a.RunID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// FinalizeArgs closes a run out once every item has reached a terminal state.
type FinalizeArgs struct {
	RunID uuid.UUID `json:"run_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (FinalizeArgs) Kind() string { return KindFinalize }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a FinalizeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDefault,
		MaxAttempts: 5,
		Metadata:    RunMetadata(a.RunID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// RunMetadata tags a River job with the run it belongs to, which is how cancellation
// finds every pending job for a run.
func RunMetadata(runID uuid.UUID) []byte {
	return []byte(fmt.Sprintf(`{"verification_run_id":%q}`, runID.String()))
}

// MetadataFilter is the JSON filter passed to river.JobListParams.Metadata.
func MetadataFilter(runID uuid.UUID) string {
	raw, _ := json.Marshal(map[string]string{"verification_run_id": runID.String()})
	return string(raw)
}
