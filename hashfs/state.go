// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"infra/build/siso/hashfs/osfs"
	pb "infra/build/siso/hashfs/proto"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/toolsupport/cogutil"
)

const defaultStateFile = ".siso_fs_state"

// OutputLocalFunc returns true if given fname needs to be on local disk.
type OutputLocalFunc func(context.Context, string) bool

// IgnoreFunc returns true if given fname should be ignored in hashfs.
type IgnoreFunc func(context.Context, string) bool

// Option is an option for HashFS.
type Option struct {
	StateFile   string
	DataSource  DataSource
	OutputLocal OutputLocalFunc
	Ignore      IgnoreFunc
	OSFSOption  osfs.Option
	CogFS       *cogutil.Client
}

// RegisterFlags registers flags for the option.
func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&o.StateFile, "fs_state", defaultStateFile, "fs state filename")
	o.OSFSOption.RegisterFlags(flagSet)
}

// DataSource is an interface to get digest source for digest and its name.
type DataSource interface {
	Source(digest.Digest, string) digest.Source
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
func Load(ctx context.Context, fname string) (*pb.State, error) {
	b, err := loadFile(ctx, fname)
	if err != nil {
		return nil, err
	}
	state := &pb.State{}
	err = proto.Unmarshal(b, state)
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

func toDigest(d *pb.Digest) digest.Digest {
	if d == nil {
		return digest.Digest{}
	}
	return digest.Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}

func fromDigest(d digest.Digest) *pb.Digest {
	if d.IsZero() {
		return nil
	}
	return &pb.Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}

// SetState sets states to the HashFS.
func (hfs *HashFS) SetState(ctx context.Context, state *pb.State) error {
	start := time.Now()
	outputLocal := hfs.opt.OutputLocal
	var neq, nnew, nnotexist, nfail, ninvalidate atomic.Int64
	var dirty atomic.Bool
	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.NumCPU())
	for i, ent := range state.Entries {
		i, ent := i, ent
		eg.Go(func() error {
			if i%1000 == 0 {
				select {
				case <-gctx.Done():
					err := context.Cause(gctx)
					clog.Errorf(gctx, "interrupted in fs.SetState: %v", err)
					return err
				default:
				}
			}
			// If cmdhash is not set, the file is a source input, not a generated output file.
			// In that case, we leave `h` empty, so we can skip this file in case it is missing
			// on disk.
			h := ent.CmdHash
			if runtime.GOOS == "windows" {
				ent.Name = strings.TrimPrefix(ent.Name, `\`)
			}
			if hfs.opt.Ignore(ctx, ent.Name) {
				clog.Infof(ctx, "ignore %s", ent.Name)
				return nil
			}
			fi, err := os.Lstat(ent.Name)
			if errors.Is(err, os.ErrNotExist) {
				if log.V(1) {
					clog.Infof(gctx, "not exist %s", ent.Name)
				}
				nnotexist.Add(1)
				if len(h) == 0 {
					clog.Infof(gctx, "not exist with no cmdhash: %s", ent.Name)
					return nil
				}
				if outputLocal(ctx, ent.Name) {
					// command output file that is needed on the disk doesn't exist on the disk.
					// need to forget to trigger steps for the output. b/298523549
					clog.Warningf(gctx, "not exist output-needed file: %s", ent.Name)
					return nil
				}

				e, _ := newStateEntry(ent, time.Time{}, hfs.opt.DataSource, hfs.OS)
				e.cmdhash = h
				e.action = toDigest(ent.Action)
				_, err = hfs.directory.store(gctx, filepath.ToSlash(ent.Name), e)
				if err != nil {
					return err
				}
				return nil
			}
			if err != nil {
				clog.Warningf(gctx, "Failed to stat %s: %v", ent.Name, err)
				nfail.Add(1)
				dirty.Store(true)
				return nil
			}
			e, et := newStateEntry(ent, fi.ModTime(), hfs.opt.DataSource, hfs.OS)
			e.cmdhash = h
			e.action = toDigest(ent.Action)
			ftype := "file"
			if e.d.IsZero() && e.target == "" {
				ftype = "dir"
				if len(e.cmdhash) == 0 {
					clog.Infof(gctx, "ignore %s %s", ftype, ent.Name)
					return nil
				}
			} else if e.d.IsZero() && e.target != "" {
				ftype = "symlink"
			} else if !e.d.IsZero() && len(h) > 0 && et != entryEqLocal && !dirty.Load() {
				// mtime differ for generated file?
				// check digest is the same and fix mtime if it matches.
				// don't reconcile for source (non-generated file),
				// as user may want to trigger build by touch.
				src := hfs.OS.FileSource(ent.Name, fi.Size())
				data, err := localDigest(ctx, src, ent.Name)
				if err == nil && data.Digest() == e.d {
					et = entryEqLocal
					err = hfs.OS.Chtimes(ctx, ent.Name, time.Now(), e.mtime)
					clog.Infof(ctx, "reconcile mtime %s %v -> %v: %v", ent.Name, fi.ModTime(), e.mtime, err)
				} else {
					clog.Warningf(ctx, "failed to reconcile mtime %s digest %s(state) != %s(local) err: %v", ent.Name, e.d, data.Digest(), err)
				}
			}
			switch et {
			case entryNoLocal:
				// it should not happen since we already checked it in `if errors.Is(err, os.ErrNotExist)` above.
				nnotexist.Add(1)
				dirty.Store(true)
				if len(h) == 0 {
					// file is a source input, not generated
					return nil
				}
				if outputLocal(ctx, ent.Name) {
					// file is a output file and needed on the disk
					return nil
				}

				clog.Infof(gctx, "not exist %s %s cmdhash:%s", ftype, ent.Name, base64.StdEncoding.EncodeToString(e.cmdhash))
			case entryBeforeLocal:
				ninvalidate.Add(1)
				dirty.Store(true)
				clog.Warningf(gctx, "invalidate %s %s: state:%s disk:%s", ftype, ent.Name, e.mtime, fi.ModTime())
				return nil
			case entryEqLocal:
				neq.Add(1)
				if log.V(1) {
					clog.Infof(gctx, "equal local %s %s: %s", ftype, ent.Name, e.mtime)
				}
			case entryAfterLocal:
				nnew.Add(1)
				dirty.Store(true)
				if len(h) == 0 {
					return nil
				}
				clog.Infof(gctx, "old local %s %s: state:%s disk:%s cmdhash:%s", ftype, ent.Name, e.mtime, fi.ModTime(), base64.StdEncoding.EncodeToString(e.cmdhash))
			}
			if log.V(1) {
				clog.Infof(gctx, "set state %s: d:%s %s s:%s m:%s cmdhash:%s action:%s", ent.Name, e.d, e.mode, e.target, e.mtime, base64.StdEncoding.EncodeToString(e.cmdhash), e.action)
			}
			_, err = hfs.directory.store(gctx, filepath.ToSlash(ent.Name), e)
			if len(e.cmdhash) > 0 {
				// records generated files found in the loaded .siso_fs_state into previouslyGeneratedFiles.
				hfs.previouslyGeneratedFiles.Store(ent.Name, true)
			}
			return err
		})
	}
	err := eg.Wait()
	if err != nil {
		return err
	}
	hfs.clean = nnew.Load() == 0 && nnotexist.Load() == 0 && nfail.Load() == 0 && ninvalidate.Load() == 0
	clog.Infof(ctx, "set state done: clean:%t eq:%d new:%d not-exist:%d fail:%d invalidate:%d: %s", hfs.clean, neq.Load(), nnew.Load(), nnotexist.Load(), nfail.Load(), ninvalidate.Load(), time.Since(start))
	return nil
}

func newStateEntry(ent *pb.Entry, ftime time.Time, dataSource DataSource, osfs *osfs.OSFS) (*entry, entryStateType) {
	lready := make(chan bool, 1)
	entTime := time.Unix(0, ent.Id.ModTime)
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
	mode := fs.FileMode(0644)
	if ent.IsExecutable {
		mode |= 0111
	}
	var dir *directory
	var src digest.Source
	entDigest := toDigest(ent.Digest)
	if !entDigest.IsZero() {
		if entType == entryEqLocal {
			src = osfs.FileSource(ent.Name, entDigest.SizeBytes)
		} else {
			// not the same as local, but digest is in state.
			// probably, exists in RBE side, or local cache.
			src = dataSource.Source(entDigest, ent.Name)
		}
	} else if ent.Target != "" {
		mode |= fs.ModeSymlink
	} else {
		dir = &directory{}
		mode |= fs.ModeDir
	}
	updatedTime := time.Unix(0, ent.UpdatedTime)
	if updatedTime.Before(entTime) {
		updatedTime = entTime
	}
	e := &entry{
		lready:      lready,
		size:        entDigest.SizeBytes,
		mtime:       entTime,
		mode:        mode,
		updatedTime: updatedTime,
		target:      ent.Target,
		src:         src,
		d:           entDigest,
		directory:   dir,
	}
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
func Save(ctx context.Context, fname string, state *pb.State) error {
	b, err := proto.Marshal(state)
	if err != nil {
		return err
	}
	return saveFile(ctx, fname, b)
}

// State returns a State of the HashFS.
func (hfs *HashFS) State(ctx context.Context) *pb.State {
	state := &pb.State{}
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
		dir.dir.m.Range(func(k, _ any) bool {
			name := filepath.Join(dir.name, k.(string))
			names = append(names, name)
			return true
		})
		sort.Strings(names)
		if log.V(1) {
			clog.Infof(ctx, "state dir=%s -> %q", dir.name, names)
		}
		for _, name := range names {
			v, ok := dir.dir.m.Load(filepath.Base(name))
			if !ok {
				clog.Errorf(ctx, "dir:%s name:%s entries:%v", dir.name, name, dir.dir)
				continue
			}
			e := v.(*entry)
			if e.err != nil {
				if log.V(1) {
					clog.Infof(ctx, "ignore %s: err:%v", name, e.err)
				}
				continue
			}
			if runtime.GOOS == "windows" {
				name = strings.TrimPrefix(name, `\`)
				if len(name) == 2 && name[1] == ':' {
					name += `\`
				}
			}
			if e.directory != nil {
				// TODO(b/253541407): record mtime for other directory?
				dirs = append(dirs, d{name: name, dir: e.directory})
			}
			if e.mtime.IsZero() {
				if len(e.cmdhash) > 0 {
					clog.Warningf(ctx, "wrong entry for %s: mtime is zero, but cmdhash set %s", name, e.cmdhash)
				} else if log.V(1) {
					clog.Infof(ctx, "ignore %s: no mtime", name)
				}
				continue
			}
			if len(e.cmdhash) > 0 {
				// need to record the entry for incremental build
				if e.directory == nil && e.target == "" && e.d.IsZero() {
					// digest is not calculated yet?
					if e.src == nil {
						clog.Warningf(ctx, "wrong entry for %s?", name)
					} else {
						err := e.compute(ctx, name)
						if err != nil {
							clog.Warningf(ctx, "failed to calculate digest for %s: %v", name, err)
						}
					}
				}
			}
			if !e.d.IsZero() || e.target != "" {
				state.Entries = append(state.Entries, &pb.Entry{
					Id: &pb.FileID{
						ModTime: e.mtime.UnixNano(),
					},
					Name:         name,
					Digest:       fromDigest(e.d),
					IsExecutable: e.mode&0111 != 0,
					Target:       e.target,
					CmdHash:      e.cmdhash,
					Action:       fromDigest(e.action),
					UpdatedTime:  e.updatedTime.UnixNano(),
				})
			} else if e.directory != nil && len(e.cmdhash) > 0 {
				// preserve dir for cmdhash
				state.Entries = append(state.Entries, &pb.Entry{
					Id: &pb.FileID{
						ModTime: e.mtime.UnixNano(),
					},
					Name:        name,
					CmdHash:     e.cmdhash,
					Action:      fromDigest(e.action),
					UpdatedTime: e.updatedTime.UnixNano(),
				})
			} else if len(e.cmdhash) > 0 {
				clog.Warningf(ctx, "wrong entry for %s: cmdhash is set, but no digest?", name)
			}
		}
	}
	return state
}

func StateMap(s *pb.State) map[string]*pb.Entry {
	m := make(map[string]*pb.Entry)
	for _, e := range s.Entries {
		m[e.Name] = e
	}
	return m
}
