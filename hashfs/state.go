// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"context"
	"errors"
	"flag"

	"infra/build/siso/reapi/digest"
)

const defaultStateFile = ".siso_fs_state"

// Option is an option for HashFS.
type Option struct {
	StateFile  string
	DataSource DataSource
}

// RegisterFlags registers flags for the option.
func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&o.StateFile, "fs_state", defaultStateFile, "fs state filename")
}

// DataSource is an interface to get digest data for digest and its name.
type DataSource interface {
	DigestData(digest.Digest, string) digest.Data
}

type fileID struct {
	ModTime int64 `json:"mtime"`
}

// State is a state of HashFS.
type State struct {
	Entries []EntryState
}

// EntryState is a state of a file entry in HashFS.
type EntryState struct {
	ID fileID `json:"id"`
	// Name is absolute filepath.
	Name         string        `json:"name"`
	Digest       digest.Digest `json:"digest,omitempty"`
	IsExecutable bool          `json:"x,omitempty"`
	// Target is symlink target.
	Target string `json:"s,omitempty"`

	// action, cmd that generated this file.
	CmdHash string        `json:"h,omitempty"`
	Action  digest.Digest `json:"action,omitempty"`
}

func (s *State) Map() map[string]EntryState {
	panic("hashfs.Map: not implemented")
}

// Load loads a HashFS's state.
func Load(ctx context.Context, fname string) (*State, error) {
	return nil, errors.New("hashfs.Load: not implemented")
}
