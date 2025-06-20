// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

// StatusReporter is an interface to report build status.
type StatusReporter interface {
	// PlanHasTotalSteps is called when total steps is updated.
	PlanHasTotalSteps(total int)

	// BuildStepStarted is called when build step started.
	BuildStepStarted(*Step)

	// BuildStepFInished is called when build step finished.
	BuildStepFinished(*Step)

	// BuildStarted is called when build started.
	BuildStarted()

	// BuildFinished is called when build finished.
	BuildFinished()
}

type noopStatusReporter struct{}

func (noopStatusReporter) PlanHasTotalSteps(total int) {}

func (noopStatusReporter) BuildStepStarted(step *Step)  {}
func (noopStatusReporter) BuildStepFinished(step *Step) {}

func (noopStatusReporter) BuildStarted()  {}
func (noopStatusReporter) BuildFinished() {}
