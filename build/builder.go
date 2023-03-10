// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

// logLabelKeyID is a key of logging label for step ID.
const logLabelKeyID = "id"

// Builder is a builder.
type Builder struct {
	stepDefs      StepDefs
	sharedDepsLog SharedDepsLog
}
