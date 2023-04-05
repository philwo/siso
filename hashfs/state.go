// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
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

func loadFile(ctx context.Context, fname string) ([]byte, error) {
	b, err := os.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	b, err = io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	err = r.Close()
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Load loads a HashFS's state.
func Load(ctx context.Context, fname string) (*State, error) {
	b, err := loadFile(ctx, fname)
	if err != nil {
		return nil, err
	}
	state := &State{}
	err = json.Unmarshal(b, state)
	if err != nil {
		return nil, err
	}
	return state, nil
}

type entryStateType int

const (
	entryNoLocal entryStateType = iota
	entryBeforeLocal
	entryEqLocal
	entryAfterLocal
)

// SetState sets states to the HashFS.
func (hfs *HashFS) SetState(ctx context.Context, state *State) error {
	start := time.Now()
	var neq, nnew, nnotexist, nfail, ninvalidate int
	for i, ent := range state.Entries {
		if i%1000 == 0 {
			select {
			case <-ctx.Done():
				clog.Errorf(ctx, "interrupted in fs.SetState: %v", ctx.Err())
				return ctx.Err()
			default:
			}
		}
		// If cmdhash is not set, the file is a source input, not a generated output file.
		// In that case, we leave `h` empty, so we can skip this file in case it is missing
		// on disk.
		var h []byte
		if ent.CmdHash != "" {
			var err error
			h, err = hex.DecodeString(ent.CmdHash)
			if err != nil {
				clog.Warningf(ctx, "Failed to decode %s cmdhash=%q: %v", ent.Name, ent.CmdHash, err)
			}
		}
		if runtime.GOOS == "windows" {
			ent.Name = strings.TrimPrefix(ent.Name, `\`)
		}
		fi, err := os.Lstat(ent.Name)
		if errors.Is(err, os.ErrNotExist) {
			if log.V(1) {
				clog.Infof(ctx, "not exist %s", ent.Name)
			}
			nnotexist++
			if len(h) == 0 {
				clog.Infof(ctx, "not exist with no cmdhash: %s", ent.Name)
				continue
			}
			e, _ := newStateEntry(ent, time.Time{}, hfs.opt.DataSource, hfs.IOMetrics)
			e.Cmdhash = h
			e.Action = ent.Action
			if err := hfs.directory.Store(ctx, filepath.ToSlash(ent.Name), e); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			clog.Warningf(ctx, "Failed to stat %s: %v", ent.Name, err)
			nfail++
			continue
		}
		e, et := newStateEntry(ent, fi.ModTime(), hfs.opt.DataSource, hfs.IOMetrics)
		e.Cmdhash = h
		e.Action = ent.Action
		ftype := "file"
		if e.D.IsZero() && e.Target == "" {
			ftype = "dir"
			clog.Infof(ctx, "ignore %s %s", ftype, ent.Name)
			continue
		} else if e.D.IsZero() && e.Target != "" {
			ftype = "symlink"
		}
		switch et {
		case entryNoLocal: // never?
			nnotexist++
			if len(h) == 0 {
				continue
			}
			clog.Infof(ctx, "not exist %s %s cmdhash:%s", ftype, ent.Name, hex.EncodeToString(e.Cmdhash))
		case entryBeforeLocal:
			ninvalidate++
			clog.Warningf(ctx, "invalidate %s %s: state:%s disk:%s", ftype, ent.Name, e.Mtime, fi.ModTime())
			continue
		case entryEqLocal:
			neq++
			if log.V(1) {
				clog.Infof(ctx, "equal local %s %s: %s", ftype, ent.Name, e.Mtime)
			}
		case entryAfterLocal:
			nnew++
			if len(h) == 0 {
				continue
			}
			clog.Infof(ctx, "old local %s %s: state:%s disk:%s cmdhash:%s", ftype, ent.Name, e.Mtime, fi.ModTime(), hex.EncodeToString(e.Cmdhash))
		}
		if log.V(1) {
			clog.Infof(ctx, "set state %s: d:%s x:%t s:%s m:%s cmdhash:%s action:%s", ent.Name, e.D, e.IsExecutable, e.Target, e.Mtime, hex.EncodeToString(e.Cmdhash), e.Action)
		}
		if err := hfs.directory.Store(ctx, filepath.ToSlash(ent.Name), e); err != nil {
			return err
		}
	}
	clog.Infof(ctx, "set state done: eq:%d new:%d not-exist:%d fail:%d invalidate:%d: %s", neq, nnew, nnotexist, nfail, ninvalidate, time.Since(start))
	return nil
}

func newStateEntry(ent EntryState, ftime time.Time, dataSource DataSource, m *iometrics.IOMetrics) (*entry, entryStateType) {
	lready := make(chan bool, 1)
	readyq := make(chan struct{})
	close(readyq)
	var data digest.Data
	entTime := time.Unix(0, ent.ID.ModTime)
	var entType entryStateType
	switch {
	case ftime.IsZero():
		// local doesn't exist
		entType = entryNoLocal
		lready <- true
	case entTime.Before(ftime):
		entType = entryBeforeLocal
		close(lready)
	case entTime.Equal(ftime):
		entType = entryEqLocal
		close(lready)
	case entTime.After(ftime):
		entType = entryAfterLocal
		lready <- true
	}
	var dir *directory
	if !ent.Digest.IsZero() {
		if entType == entryEqLocal {
			data = digest.NewData(digest.LocalFileSource{Fname: ent.Name, IOMetrics: m}, ent.Digest)
		} else {
			// not the same as local, but digest is in state.
			// probably, exists in RBE side, or local cache.
			data = dataSource.DigestData(ent.Digest, ent.Name)
		}
	} else if ent.Target == "" {
		dir = &directory{}
	}
	e := &entry{
		Lready:       lready,
		Mtime:        entTime,
		Readyq:       readyq,
		D:            ent.Digest,
		IsExecutable: ent.IsExecutable,
		Target:       ent.Target,
		Data:         data,
		Directory:    dir,
	}
	e.Ready.Store(true)
	return e, entType
}

func saveFile(ctx context.Context, fname string, data []byte) error {
	// save old state in *.0
	ofname := fname + ".0"
	if err := os.Remove(ofname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(fname, ofname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	w, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := w.Write(data); err != nil {
		f.Close()
		return err
	}
	err = w.Close()
	if err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Save persists state in fname.
func Save(ctx context.Context, fname string, state *State) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return saveFile(ctx, fname, b)
}

// State returns a State of the HashFS.
func (hfs *HashFS) State(ctx context.Context) *State {
	state := &State{}
	type d struct {
		name string
		dir  *directory
	}
	var dirs []d
	dirs = append(dirs, d{name: "/", dir: hfs.directory})
	for len(dirs) > 0 {
		dir := dirs[0]
		dirs = dirs[1:]
		var names []string
		if log.V(1) {
			clog.Infof(ctx, "state dir=%s dirs=%d", dir.name, len(dirs))
		}
		// TODO(b/254182269): need mutex here?
		dir.dir.M.Range(func(k, _ any) bool {
			name := filepath.Join(dir.name, k.(string))
			names = append(names, name)
			return true
		})
		sort.Strings(names)
		if log.V(1) {
			clog.Infof(ctx, "state dir=%s -> %q", dir.name, names)
		}
		for _, name := range names {
			v, ok := dir.dir.M.Load(filepath.Base(name))
			if !ok {
				clog.Errorf(ctx, "dir:%s name:%s entries:%v", dir.name, name, dir.dir)
				continue
			}
			e := v.(*entry)
			if err := e.GetError(); err != nil {
				if log.V(1) {
					clog.Infof(ctx, "ignore %s: err:%v", name, err)
				}
				continue
			}
			if runtime.GOOS == "windows" {
				name = strings.TrimPrefix(name, `\`)
				if len(name) == 2 && name[1] == ':' {
					name += `\`
				}
			}
			if e.Mtime.IsZero() {
				if log.V(1) {
					clog.Infof(ctx, "ignore %s: no mtime", name)
				}
			} else {
				// ignore directory
				// TODO(b/253541407): record mtime for directory?
				if !e.D.IsZero() || e.Target != "" {
					state.Entries = append(state.Entries, EntryState{
						ID: fileID{
							ModTime: e.Mtime.UnixNano(),
						},
						Name:         name,
						Digest:       e.D,
						IsExecutable: e.IsExecutable,
						Target:       e.Target,
						CmdHash:      hex.EncodeToString(e.Cmdhash),
						Action:       e.Action,
					})
				}
			}
			if e.Directory != nil {
				dirs = append(dirs, d{name: name, dir: e.Directory})
			}
		}
	}
	return state
}

func (s *State) Map() map[string]EntryState {
	m := make(map[string]EntryState)
	for _, e := range s.Entries {
		m[e.Name] = e
	}
	return m
}
