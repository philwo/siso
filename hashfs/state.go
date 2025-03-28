// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/biogo/hts/bgzf"
	"github.com/charmbracelet/log"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/infra/build/siso/hashfs/osfs"
	pb "go.chromium.org/infra/build/siso/hashfs/proto"
	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/runtimex"
	"go.chromium.org/infra/build/siso/toolsupport/artfsutil"
	"go.chromium.org/infra/build/siso/toolsupport/cogutil"
)

const defaultStateFile = ".siso_fs_state"

// defaultCompressThreads is the default number of threads to use for data
// compression. Using more than 8 threads is unlikely to provide any benefit
// due to coordination overhead and contention
var defaultCompressThreads = min(8, runtime.GOMAXPROCS(0))

// OutputLocalFunc returns true if given fname needs to be on local disk.
type OutputLocalFunc func(context.Context, string) bool

// IgnoreFunc returns true if given fname should be ignored in hashfs.
type IgnoreFunc func(context.Context, string) bool

// Option is an option for HashFS.
type Option struct {
	StateFile       string // filename that HashFS saves its state to
	GzipUsesBgzf    bool   // use bgzf for gzip compression
	CompressZstd    bool   // compress fs state using zstd instead of gzip
	CompressLevel   int    // compression level (0 = uncompressed, 1 = fastest, 10 = best)
	CompressThreads int    // number of threads to use for data compression

	KeepTainted bool // keep manually modified generated file

	FSMonitor FSMonitor

	DataSource  DataSource
	OutputLocal OutputLocalFunc
	Ignore      IgnoreFunc
	CogFS       *cogutil.Client
	ArtFS       *artfsutil.Client

	SetStateLogger io.Writer // capture SetState log for test
}

// RegisterFlags registers flags for the option.
func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&o.StateFile, "fs_state", defaultStateFile, "fs state filename")
	flagSet.BoolVar(&o.GzipUsesBgzf, "fs_state_use_bgzf", true, "use bgzf for gzip compression")
	flagSet.BoolVar(&o.CompressZstd, "fs_state_use_zstd", false, "compress fs state using zstd instead of gzip")
	flagSet.IntVar(&o.CompressLevel, "fs_state_compression_level", 3, "fs state compression level (0 = uncompressed, 1 = fastest, 10 = best)")
	flagSet.IntVar(&o.CompressThreads, "fs_state_compression_threads", defaultCompressThreads, "number of threads to use for data compression")
	flagSet.BoolVar(&o.KeepTainted, "fs_keep_tainted", false, "keep manually modified generated file")
}

// DataSource is an interface to get digest source for digest and its name.
type DataSource interface {
	Source(context.Context, digest.Digest, string) digest.Source
}

func isGzip(b []byte) bool {
	// Files compressed with gzip always start with the magic bytes 0x1f 0x8b.
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

func isZstd(b []byte) bool {
	// Files compressed with zstd always start with the magic bytes 0x28 0xb5 0x2f 0xfd.
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

func loadFile(opts Option) ([]byte, error) {
	f, err := os.Open(opts.StateFile)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Warnf("Failed to close %s: %v", opts.StateFile, err)
		}
	}()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// The first 4 bytes of the file are enough to determine the compression format.
	magicBytes := make([]byte, 4)
	if _, err := io.ReadFull(f, magicBytes); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var r io.ReadCloser
	if isZstd(magicBytes) {
		log.Infof("fs_state is zstd compressed")
		var zd *zstd.Decoder
		zd, err = zstd.NewReader(f)
		if err != nil {
			return nil, err
		}
		r = zd.IOReadCloser()
	} else if isGzip(magicBytes) {
		log.Infof("fs_state is gzip compressed")
		if opts.GzipUsesBgzf {
			r, err = bgzf.NewReader(f, 0)
			if err == nil {
				log.Infof("using bgzf for faster gzip decompression")
			} else if errors.Is(err, bgzf.ErrNoBlockSize) {
				// bgzf refuses to decompress regular gzip files, so we need to
				// check for this case and retry with a regular gzip reader.
				log.Infof("not bgzf, retrying as regular gzip")
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
				r, err = gzip.NewReader(f)
			}
		} else {
			r, err = gzip.NewReader(f)
		}
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("unknown compression format, neither gzip nor zstd?")
	}

	// Unfortunately, neither zstd nor gzip are able to provide the uncompressed size
	// of the data. However, it's a safe assumption that the uncompressed size is at
	// least as large as the compressed size (and even if not we're only wasting a
	// few bytes of memory).
	b := bytes.NewBuffer(make([]byte, 0, fi.Size()))

	if _, err = io.Copy(b, r); err != nil {
		_ = r.Close()
		return nil, err
	}

	if err = r.Close(); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// Load loads a HashFS's state.
func Load(opts Option) (*pb.State, error) {
	start := time.Now()
	b, err := loadFile(opts)
	if err != nil {
		return nil, err
	}
	durUncompress := time.Since(start)

	start = time.Now()
	state := &pb.State{}
	err = proto.Unmarshal(b, state)
	if err != nil {
		return nil, err
	}
	durUnmarshal := time.Since(start)
	log.Infof("Load fs state from %s: read/uncompress %s + unmarshal %s = total %s", opts.StateFile, durUncompress, durUnmarshal, durUncompress+durUnmarshal)

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
	logw := hfs.opt.SetStateLogger
	if logw != nil {
		fmt.Fprintf(logw, "hashfs.SetState\n")
		defer fmt.Fprintf(logw, "hashfs.SetState done\n")
	}
	if state.BuildTargets != nil {
		hfs.buildTargets = make([]string, len(state.BuildTargets.Targets))
		copy(hfs.buildTargets, state.BuildTargets.Targets)
	} else {
		hfs.buildTargets = nil
	}
	var fsm FileInfoer = osfsInfoer{}
	if hfs.opt.FSMonitor != nil && state.LastChecked != "" {
		f, err := hfs.opt.FSMonitor.Scan(ctx, state.LastChecked)
		if err != nil {
			log.Warnf("failed to fsmonitor scan %q: %v", state.LastChecked, err)
		} else {
			log.Infof("use fsmonitor scan %q", state.LastChecked)
			if logw != nil {
				fmt.Fprintf(logw, "use fsmonitor scan %q\n", state.LastChecked)
			}
			fsm = f
		}
	}
	outputLocal := hfs.opt.OutputLocal
	var neq, nnew, nnotexist, nfail, ninvalidate atomic.Int64
	var dirty atomic.Bool
	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtimex.NumCPU())
	dirs := make([]*entry, len(state.Entries))
	entries := make([]*entry, len(state.Entries))
	prevGenerated := make([]bool, len(state.Entries))
	tainted := make([]bool, len(state.Entries))
	for i, ent := range state.Entries {
		eg.Go(func() error {
			if i%1000 == 0 {
				select {
				case <-gctx.Done():
					err := context.Cause(gctx)
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
			ent.Name = filepath.ToSlash(ent.Name)
			if hfs.opt.Ignore(ctx, ent.Name) {
				if logw != nil {
					fmt.Fprintf(logw, "ignore %q\n", ent.Name)
				}
				return nil
			}
			fi, err := fsm.FileInfo(ctx, ent)
			if errors.Is(err, fs.ErrNotExist) {
				nnotexist.Add(1)
				if len(h) == 0 {
					if logw != nil {
						fmt.Fprintf(logw, "not exist with no cmd hash: %q\n", ent.Name)
					}
					return nil
				}
				if outputLocal(ctx, ent.Name) {
					// command output file that is needed on the disk doesn't exist on the disk.
					// need to forget to trigger steps for the output. b/298523549
					log.Warnf("not exist output-needed file: %q", ent.Name)
					if logw != nil {
						fmt.Fprintf(logw, "not exist output-needed file: %q\n", ent.Name)
					}
					return nil
				}
				e, _ := newStateEntry(ctx, ent, time.Time{}, hfs.opt.DataSource)
				e.cmdhash = h
				e.action = toDigest(ent.Action)
				entries[i] = e
				if logw != nil {
					fmt.Fprintf(logw, "not exist with cmd hash: %q\n", ent.Name)
				}
				return nil
			}
			if err != nil {
				log.Warnf("Failed to stat %q: %v", ent.Name, err)
				nfail.Add(1)
				dirty.Store(true)
				if logw != nil {
					fmt.Fprintf(logw, "failed to stat %q: %v\n", ent.Name, err)
				}
				return nil
			}
			if now := time.Now(); fi.ModTime().After(now) {
				log.Warnf("future timestamp on %q: mtime=%s now=%s", ent.Name, fi.ModTime(), now)
				return fmt.Errorf("future timestamp on %q: mtime=%s now=%s", ent.Name, fi.ModTime(), now)
			}
			e, et := newStateEntry(ctx, ent, fi.ModTime(), hfs.opt.DataSource)
			e.cmdhash = h
			e.action = toDigest(ent.Action)
			ftype := "file"
			if e.d.IsZero() && e.target == "" {
				ftype = "dir"
				if len(e.cmdhash) == 0 {
					if logw != nil {
						fmt.Fprintf(logw, "ignore dir no cmd hash: %q\n", ent.Name)
					}
					return nil
				}
			} else if e.d.IsZero() && e.target != "" {
				ftype = "symlink"
				t, err := os.Readlink(ent.Name)
				if err != nil {
					log.Warnf("failed to readlink %q: %v", ent.Name, err)
					nfail.Add(1)
					dirty.Store(true)
					if logw != nil {
						fmt.Fprintf(logw, "failed to readlink %q: %v\n", ent.Name, err)
					}
					return nil
				}
				if t != e.target {
					log.Warnf("invalidate %s %q: target:%q->%q", ftype, ent.Name, e.target, t)
					ninvalidate.Add(1)
					dirty.Store(true)
					if logw != nil {
						fmt.Fprintf(logw, "invalidate symlink %q: target: %q->%q\n", ent.Name, e.target, t)
					}
					return nil
				}
			} else if !e.d.IsZero() && len(h) > 0 && et != entryEqLocal && !dirty.Load() {
				// mtime differ for generated file?
				// check digest is the same and fix mtime if it matches.
				// don't reconcile for source (non-generated file),
				// as user may want to trigger build by touch.
				src := osfs.NewFileSource(ent.Name)
				data, err := localDigest(ctx, src, ent.Name)
				if err == nil && data.Digest() == e.d {
					et = entryEqLocal
					err = os.Chtimes(ent.Name, time.Now(), e.mtime)
					if logw != nil {
						fmt.Fprintf(logw, "reconcile mtime %q %v -> %v: %v\n", ent.Name, fi.ModTime(), e.mtime, err)
					}
				} else {
					log.Warnf("failed to reconcile mtime %q digest %s(state) != %s(local) err: %v", ent.Name, e.d, data.Digest(), err)
					if logw != nil {
						fmt.Fprintf(logw, "failed to reconcile mtime %q digest mismatch\n", ent.Name)
					}
				}
			}
			switch et {
			case entryNoLocal:
				// it should not happen since we already checked it in `if errors.Is(err, os.ErrNotExist)` above.
				nnotexist.Add(1)
				dirty.Store(true)
				if len(h) == 0 {
					// file is a source input, not generated
					if logw != nil {
						fmt.Fprintf(logw, "no local entry source: %q\n", ent.Name)
					}
					return nil
				}
				if outputLocal(ctx, ent.Name) {
					// file is a output file and needed on the disk
					if logw != nil {
						fmt.Fprintf(logw, "no local output local: %s %q\n", ftype, ent.Name)
					}
					return nil
				}

				if logw != nil {
					fmt.Fprintf(logw, "no local output: %s %q\n", ftype, ent.Name)
				}
			case entryBeforeLocal:
				ninvalidate.Add(1)
				dirty.Store(true)
				log.Warnf("invalidate %s %q: state:%s disk:%s", ftype, ent.Name, e.mtime, fi.ModTime())
				if h == nil || !hfs.opt.KeepTainted {
					if logw != nil {
						fmt.Fprintf(logw, "invalidate %s %q: state:%s disk:%s\n", ftype, ent.Name, e.mtime, fi.ModTime())
					}
					return nil
				}
				tainted[i] = true
				if logw != nil {
					fmt.Fprintf(logw, "keep tainted %s %q: state:%s disk:%s\n", ftype, ent.Name, e.mtime, fi.ModTime())
				}
				// keep this entry to preserve cmdhash
				// but use mtime of actual file.
				le := newLocalEntry()
				le.init(ent.Name, hfs.executables)
				le.cmdhash = e.cmdhash
				le.action = e.action
				e = le
			case entryEqLocal:
				neq.Add(1)
				if logw != nil {
					fmt.Fprintf(logw, "equal local %s %q: %s\n", ftype, ent.Name, e.mtime)
				}
			case entryAfterLocal:
				nnew.Add(1)
				dirty.Store(true)
				if len(h) == 0 {
					if logw != nil {
						fmt.Fprintf(logw, "old local source %s %q: state:%s disk:%s\n", ftype, ent.Name, e.mtime, fi.ModTime())
					}
					return nil
				}
				if logw != nil {
					fmt.Fprintf(logw, "old local %s %q: state:%s disk:%s cmdhash:%s\n", ftype, ent.Name, e.mtime, fi.ModTime(), base64.StdEncoding.EncodeToString(e.cmdhash))
				}
			}
			if ftype == "dir" {
				dirs[i] = e
			} else {
				entries[i] = e
			}
			if len(e.cmdhash) > 0 {
				// records generated files found in the loaded .siso_fs_state into previouslyGeneratedFiles.
				prevGenerated[i] = true
			}
			return err
		})
	}
	err := eg.Wait()
	if err != nil {
		log.Warnf("failed in SetState: %v", err)
		return err
	}
	for i, ent := range state.Entries {
		if prevGenerated[i] {
			hfs.previouslyGeneratedFiles = append(hfs.previouslyGeneratedFiles, ent.Name)
		}
		if tainted[i] {
			hfs.taintedFiles = append(hfs.taintedFiles, ent.Name)
		}
	}
	hfs.setStateCh = make(chan error, 1)
	clean := nnew.Load() == 0 && nnotexist.Load() == 0 && nfail.Load() == 0 && ninvalidate.Load() == 0
	hfs.clean.Store(clean)
	// store in background.
	go func() {
		defer close(hfs.setStateCh)
		// name is sorted in state.Entries.

		// store dir early.
		// otherwise, flaky confirm no-op failure
		// for step that outputs dir and dir/file.
		// i.e. if dir/file is stored before dir,
		// dir/file's cmdhash etc will be lost.
		for i, ent := range state.Entries {
			e := dirs[i]
			if e == nil {
				continue
			}
			_, err := hfs.directory.store(ctx, ent.Name, e)
			if err != nil {
				hfs.clean.Store(false)
				hfs.setStateCh <- fmt.Errorf("failed to store dir %s: %w", ent.Name, err)
				return
			}
		}
		for i, ent := range state.Entries {
			e := entries[i]
			if e == nil {
				continue
			}
			_, err := hfs.directory.store(ctx, ent.Name, e)
			if err != nil {
				hfs.clean.Store(false)
				hfs.setStateCh <- fmt.Errorf("failed to store file %s: %w", ent.Name, err)
				return
			}
		}
		hfs.loaded.Store(true)
		log.Infof("set state done: clean:%t loaded:true: %s", hfs.clean.Load(), time.Since(start))
		hfs.setStateCh <- nil
	}()
	log.Infof("load state done: eq:%d new:%d not-exist:%d fail:%d invalidate:%d: tainted:%d %s", neq.Load(), nnew.Load(), nnotexist.Load(), nfail.Load(), ninvalidate.Load(), len(hfs.taintedFiles), time.Since(start))
	return nil
}

func newStateEntry(ctx context.Context, ent *pb.Entry, ftime time.Time, dataSource DataSource) (*entry, entryStateType) {
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
			src = osfs.NewFileSource(ent.Name)
		} else {
			// not the same as local, but digest is in state.
			// probably, exists in RBE side, or local cache.
			src = dataSource.Source(ctx, entDigest, ent.Name)
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

func saveFile(data []byte, opts Option) (retErr error) {
	compressThreads := opts.CompressThreads
	if compressThreads == 0 {
		compressThreads = defaultCompressThreads
	}

	f, err := os.CreateTemp(filepath.Dir(opts.StateFile), filepath.Base(opts.StateFile)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(f.Name())
		}
	}()
	log.Infof("save fs_state in temp %s", f.Name())
	var w io.WriteCloser
	if opts.CompressZstd {
		log.Infof("using zstd compression (level %d)", opts.CompressLevel)
		opts := []zstd.EOption{
			zstd.WithEncoderCRC(true),
			zstd.WithEncoderConcurrency(compressThreads),
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(opts.CompressLevel)),
			zstd.WithZeroFrames(true),
		}
		w, err = zstd.NewWriter(f, opts...)
	} else {
		log.Infof("using gzip compression (level %d)", opts.CompressLevel)
		if opts.GzipUsesBgzf {
			log.Infof("using bgzf for faster gzip compression (threads=%d)", compressThreads)
			w, err = bgzf.NewWriterLevel(f, opts.CompressLevel, compressThreads)
		} else {
			w, err = gzip.NewWriterLevel(f, opts.CompressLevel)
		}
	}
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
	err = f.Close()
	if err != nil {
		return err
	}
	// save old state in *.0
	ofname := opts.StateFile + ".0"
	if err := os.Remove(ofname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(opts.StateFile, ofname); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err = os.Rename(f.Name(), opts.StateFile)
	return err
}

// Save persists state in fname.
func Save(state *pb.State, opts Option) error {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		// state is broken?? panic in proto.Marshal b/323265794
		log.Errorf("state: %d entries", len(state.Entries))
		for i, ent := range state.Entries {
			err := func(ent *pb.Entry) (err error) {
				defer func() {
					r := recover()
					if r != nil {
						err = fmt.Errorf("panic in marshal: %v", r)
					}
				}()
				_, err = proto.Marshal(ent)
				return err
			}(ent)
			log.Errorf("entries[%d] = %v: %v", i, ent, err)
		}
		panic(r)
	}()
	start := time.Now()
	b, err := proto.Marshal(state)
	if err != nil {
		return err
	}
	durMarshal := time.Since(start)

	start = time.Now()
	err = saveFile(b, opts)
	if err != nil {
		return err
	}
	durSave := time.Since(start)

	log.Infof("Save fs state to %s: marshal %s + compress/save %s = total %s", opts.StateFile, durMarshal, durSave, durMarshal+durSave)

	// Journal data are already included in state.
	// Remove journal file as it is not needed to reconcile in next build.
	err = os.Remove(opts.StateFile + ".journal")
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// State returns a State of the HashFS.
func (hfs *HashFS) State(ctx context.Context) *pb.State {
	started := time.Now()
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
		// TODO(b/254182269): need mutex here?
		dir.dir.m.Range(func(k, _ any) bool {
			name := filepath.ToSlash(filepath.Join(dir.name, k.(string)))
			names = append(names, name)
			return true
		})
		sort.Strings(names)
		for _, name := range names {
			v, ok := dir.dir.m.Load(filepath.Base(name))
			if !ok {
				log.Errorf("dir:%s name:%s entries:%v", dir.name, name, dir.dir)
				continue
			}
			e := v.(*entry)
			if e.err != nil {
				continue
			}
			if runtime.GOOS == "windows" {
				name = strings.TrimPrefix(name, "/")
				if len(name) == 2 && name[1] == ':' {
					name += `/`
				}
			}
			if e.directory != nil {
				// TODO(b/253541407): record mtime for other directory?
				dirs = append(dirs, d{name: name, dir: e.directory})
			}
			if e.mtime.IsZero() {
				if len(e.cmdhash) > 0 {
					log.Warnf("wrong entry for %s: mtime is zero, but cmdhash set %s", name, e.cmdhash)
				}
				continue
			}
			if len(e.cmdhash) > 0 {
				// need to record the entry for incremental build
				if e.directory == nil && e.target == "" && e.d.IsZero() {
					// digest is not calculated yet?
					if e.src == nil {
						log.Warnf("wrong entry for %s?", name)
					} else {
						err := e.compute(ctx, name)
						if err != nil {
							log.Warnf("failed to calculate digest for %s: %v", name, err)
						}
					}
				}
			}
			if !e.d.IsZero() || e.target != "" {
				e.mu.Lock()
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
				e.mu.Unlock()
			} else if e.directory != nil && len(e.cmdhash) > 0 {
				// preserve dir for cmdhash
				e.mu.Lock()
				state.Entries = append(state.Entries, &pb.Entry{
					Id: &pb.FileID{
						ModTime: e.mtime.UnixNano(),
					},
					Name:        name,
					CmdHash:     e.cmdhash,
					Action:      fromDigest(e.action),
					UpdatedTime: e.updatedTime.UnixNano(),
				})
				e.mu.Unlock()
			} else if len(e.cmdhash) > 0 {
				log.Warnf("wrong entry for %s: cmdhash is set, but no digest?", name)
			}
		}
	}
	if hfs.opt.FSMonitor != nil {
		token, err := hfs.opt.FSMonitor.ClockToken(ctx)
		if err != nil {
			log.Warnf("failed to get fsmonitor token: %v", err)
		} else {
			log.Infof("fsmonitor last checked = %q", token)
			state.LastChecked = token
		}
	}
	if hfs.buildTargets != nil {
		state.BuildTargets = &pb.BuildTargets{
			Targets: hfs.buildTargets,
		}
	}
	log.Infof("state %d entries token:%q buildTargets:%v: %s", len(state.Entries), state.LastChecked, state.BuildTargets, time.Since(started))
	return state
}

func StateMap(s *pb.State) map[string]*pb.Entry {
	m := make(map[string]*pb.Entry)
	for _, e := range s.Entries {
		m[e.Name] = e
	}
	return m
}

func loadJournal(fname string, state *pb.State) bool {
	started := time.Now()
	b, err := os.ReadFile(fname)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Infof("no fs state journal: %v", err)
		} else {
			log.Warnf("Failed to load journal: %v", err)
		}
		return false
	}
	var cnt int
	var broken bool
	m := StateMap(state)
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		ent := &pb.Entry{}
		err := dec.Decode(&ent)
		if err != nil {
			log.Warnf("Failed to decode journal: %v", err)
			broken = true
			break
		}
		m[ent.Name] = ent
		cnt++
	}
	if cnt == 0 {
		return false
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	state.Entries = make([]*pb.Entry, 0, len(keys))
	for _, k := range keys {
		state.Entries = append(state.Entries, m[k])
	}
	log.Infof("reconcile from journal %d entries (broken=%t) in %s", cnt, broken, time.Since(started))
	return true
}

func (hfs *HashFS) journalEntry(ctx context.Context, fname string, e *entry) {
	// If there are any tainted files, don't journal the entry.
	// This is because trainted files may have been modified by the user,
	// i.e. not by generated by the build from the source,
	// and we don't want to overwrite their changes with the state from
	// the journal.
	if len(hfs.taintedFiles) > 0 {
		return
	}
	if e.digest().IsZero() {
		hfs.digester.compute(ctx, fname, e)
	}
	e.mu.Lock()
	ent := &pb.Entry{
		Id: &pb.FileID{
			ModTime: e.mtime.UnixNano(),
		},
		Name:         fname,
		Digest:       fromDigest(e.d),
		IsExecutable: e.mode&0111 != 0,
		Target:       e.target,
		CmdHash:      e.cmdhash,
		Action:       fromDigest(e.action),
		UpdatedTime:  e.updatedTime.UnixNano(),
	}
	e.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(ent)
	if err != nil {
		log.Warnf("Failed to marshal journal entry %s: %v", fname, err)
		return
	}
	hfs.journalMu.Lock()
	defer hfs.journalMu.Unlock()
	if hfs.journal == nil {
		return
	}
	_, err = hfs.journal.Write(buf.Bytes())
	if err != nil {
		log.Warnf("Failed to write journal entry %s: %v", fname, err)
	}
}
