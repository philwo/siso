// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"
)

// IntervalMetric is a time duration, but serialized as seconds in JSON.
type IntervalMetric time.Duration

// MarshalJSON marshals the IntervalMetric as float64 of seconds.
func (i IntervalMetric) MarshalJSON() ([]byte, error) {
	d := time.Duration(i)
	secs := d.Seconds()
	return json.Marshal(secs)
}

// UnmarshalJSON unmarshals float64 of seconds as an IntervalMetric.
func (i *IntervalMetric) UnmarshalJSON(b []byte) error {
	var secs float64
	err := json.Unmarshal(b, &secs)
	if err != nil {
		return err
	}
	*i = IntervalMetric(time.Duration(int64(secs * 1e9)))
	return nil
}

// StepMetric contains metrics about a build Step.
type StepMetric struct {
	BuildID string `json:"build_id"` // the unique id of the current build (trace)
	StepID  string `json:"step_id"`  // the unique id of this step (top span)

	Rule   string `json:"rule,omitempty"` // the rule name of the step
	Action string `json:"action"`         // the action name of the step
	Output string `json:"output"`         // the name of the first output file of the step

	// The ID and name of the first output of the previous step.
	// The "previous" step is defined as the last step that updated
	// the output that is used as part of this step's inputs.
	PrevStepID  string `json:"prev,omitempty"`
	PrevStepOut string `json:"prev_out,omitempty"`

	// Ready, Start and Duration are measured by Siso's scheduler,
	// independently of the measurements provided by the execution
	// strategies (see RunTime, QueueTime, ExecTime below).

	// Ready is the time it took since build start until the action became
	// ready for execution (= all inputs are available).
	Ready IntervalMetric `json:"ready"`
	// Start is the time it took until Siso's scheduler was ready to work
	// on the step (concurrency limited by stepSema) and pass it to an
	// execution strategy.
	Start IntervalMetric `json:"start"`
	// Duration is the time it took for the action to do its job, measured
	// from start of execution until it exited.
	Duration IntervalMetric `json:"duration"`

	// WeightedDuration is an estimate of the "true duration" of the action
	// that tries to accommodate for the impact of other actions running in
	// parallel. It is calculated by summing up small slices of time (~100ms)
	// while the action is running, where each slice's duration is divided by
	// the number of concurrently running actions at that point in time.
	WeightedDuration IntervalMetric `json:"weighted_duration"`

	// The hash of the command-line of the build step.
	CmdHash string `json:"cmdhash"`
	// The hash of the action proto of this build step.
	Digest string `json:"digest"`

	DepsLog  bool `json:"deps_log"`  // whether the action used the deps log.
	IsRemote bool `json:"is_remote"` // whether the action ran remotely.
	Cached   bool `json:"cached"`    // whether the action was a cache hit.
	Fallback bool `json:"fallback"`  // whether the action failed remotely and was retried locally.
	Err      bool `json:"err"`       // whether the action failed.

	// RunTime, QueueTime and ExecTime are measured by the execution
	// strategies and

	// RunTime is the total duration of the action execution, including
	// overhead such as uploading / downloading files.
	RunTime IntervalMetric `json:"run"`
	// QueueTime is the time it took until the worker could begin executing
	// the action.
	QueueTime IntervalMetric `json:"queue"`
	// ExecTime is the time measured from the execution strategy starting
	// the process until the process exited.
	ExecTime IntervalMetric `json:"exec"`

	Inputs  int `json:"inputs"`  // how many input files.
	Outputs int `json:"outputs"` // how many output files.

	skip bool // whether the step was skipped during the build.
}

func (m *StepMetric) done(ctx context.Context, step *Step) {
	m.WeightedDuration = IntervalMetric(step.getWeightedDuration())
	m.Inputs = len(step.cmd.Inputs)
	m.Outputs = len(step.cmd.Outputs)

	m.CmdHash = hex.EncodeToString(step.cmd.CmdHash)
	m.Digest = step.cmd.ActionDigest().String()

	result, cached := step.cmd.ActionResult()
	m.Cached = cached
	md := result.GetExecutionMetadata()
	if !m.Cached {
		m.QueueTime = IntervalMetric(md.GetWorkerStartTimestamp().AsTime().Sub(md.GetQueuedTimestamp().AsTime()))
	}
	m.ExecTime = IntervalMetric(md.GetExecutionCompletedTimestamp().AsTime().Sub(md.GetExecutionStartTimestamp().AsTime()))
}
