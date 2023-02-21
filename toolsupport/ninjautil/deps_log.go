// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/golang/glog"

	"infra/build/siso/o11y/clog"
)

// TODO(b/267409605): Add tests.

// DepsLog is an in-memory representation of ninja's depslog.
// It supports creating new depslog files and reading existing depslog files,
// as well as adding new records to open depslog files.
// Format:
// https://github.com/ninja-build/ninja/blob/87111bff382655075f2577c591745a335f0103c7/src/deps_log.h
type DepsLog struct {
	fname string

	mu      sync.Mutex
	paths   []string
	pathIdx map[string]int
	deps    []*depsRecord

	w *os.File
}

const fileSignature = "# ninjadeps\n"
const currentVersion = 3
const maxRecordSize = 1<<19 - 1

// record length.
// high bit indicates record type.
//
//	unset - path record
//	set   - deps record
//
// max record sizes are capped at 512kB
type recordHeader int32

func (h recordHeader) IsDepsRecord() bool {
	return (h >> 31) != 0
}

func (h recordHeader) RecordSize() int {
	return int(h) & 0x7FFFFFF
}

type depsRecord struct {
	mtime  int32
	inputs []string
}

// NewDepsLog reads or creates a new deps log.
// If there are read errors, returns a truncated deps log.
// TODO(ukai): recompact the deps log.
func NewDepsLog(ctx context.Context, fname string) (*DepsLog, error) {
	if fname == "" {
		return nil, errors.New("no ninja_deps")
	}
	depsLog := &DepsLog{fname: fname, pathIdx: make(map[string]int)}
	fbuf, err := os.ReadFile(fname)
	if os.IsNotExist(err) {
		clog.Infof(ctx, "ninja_deps %s doesn't exist: %v", fname, err)
		createNewDepsLogFile(ctx, fname)
		return depsLog, nil
	}
	if err != nil {
		return nil, err
	}
	f := bytes.NewReader(fbuf)
	buf := make([]byte, len(fileSignature))
	n, err := f.Read(buf)
	if log.V(3) {
		clog.Infof(ctx, "signature=%q: %d %v", buf, n, err)
	}
	if err != nil || n != len(buf) {
		return nil, fmt.Errorf("failed to read file signature=%d: %v", n, err)
	}
	if !bytes.Equal(buf, []byte(fileSignature)) {
		return nil, fmt.Errorf("wrong signature %q", buf)
	}
	var ver int32
	err = binary.Read(f, binary.LittleEndian, &ver)
	if log.V(3) {
		clog.Infof(ctx, "version=%d: %v", ver, err)
	}
	if err != nil || ver != currentVersion {
		return nil, fmt.Errorf("wrong version %d", ver)
	}
	var offset int64
	// TODO(ukai): this reflects original ninja impl, may not be optimal
	buf = make([]byte, maxRecordSize+1)
	// Perform read loop. Truncates with current state of deps log if error
	// encountered while parsing (i.e. broken input should never cause error)
	// TODO(ukai): recompact the deps log.
readLoop:
	for {
		offset, err = f.Seek(0, os.SEEK_CUR)
		if log.V(3) {
			clog.Infof(ctx, "offset=%d: %v", offset, err)
		}
		if err != nil {
			clog.Errorf(ctx, "failed to get offset: %v", err)
			break readLoop
		}
		var header recordHeader
		err = binary.Read(f, binary.LittleEndian, &header)
		if log.V(3) {
			clog.Infof(ctx, "header=0x%0x: %v", header, err)
		}
		if err != nil {
			if err == io.EOF {
				break readLoop
			}
			clog.Errorf(ctx, "failed to read header at %d: %v", offset, err)
			break readLoop
		}
		size := header.RecordSize()
		if size > maxRecordSize {
			clog.Errorf(ctx, "too large record %d at %d", size, offset)
			break readLoop
		}
		n, err := f.Read(buf[:size])
		if err != nil || n != size {
			clog.Errorf(ctx, "failed to read record %d at %d: n=%d, %v", size, offset, n, err)
			break readLoop
		}
		if header.IsDepsRecord() {
			// dependency record
			// array of 4-byte integers
			//   output path id
			//   output path mtime
			//   input path id, ...
			rec := make([]int32, size/4)
			err = binary.Read(bytes.NewReader(buf[:size]), binary.LittleEndian, rec)
			if log.V(3) {
				clog.Infof(ctx, "deps record=%v: %v", rec, err)
			}
			if err != nil {
				clog.Errorf(ctx, "failed to parse deps record at %d: %v", offset, err)
				break readLoop
			}
			outID := rec[0]
			mtime := rec[1]
			rec = rec[2:]
			deps := &depsRecord{mtime: mtime, inputs: make([]string, 0, len(rec))}
			for _, id := range rec {
				if int(id) < 0 || int(id) >= len(depsLog.paths) {
					clog.Warningf(ctx, "bad path id=%d (depsLog.paths=%d)", id, len(depsLog.paths))
					continue
				}
				deps.inputs = append(deps.inputs, depsLog.paths[id])
			}
			depsLog.update(ctx, outID, deps)
			continue
		}
		// path record
		//  string name of the path
		//  up to 3 padding bytes to align on 4 byte boundaries
		//  one's complement of the expected index of the record
		//  checksum (4 bytes)
		pathSize := size - 4
		for i := 0; i < 3; i++ {
			if buf[pathSize-1] == 0 {
				pathSize--
			}
		}
		pathname := string(buf[:pathSize])
		if log.V(3) {
			clog.Infof(ctx, "path record %q %d", pathname, len(depsLog.paths))
		}

		var checksum int32
		err = binary.Read(bytes.NewReader(buf[size-4:size]), binary.LittleEndian, &checksum)
		if log.V(3) {
			clog.Infof(ctx, "checksum %x: %v", checksum, err)
		}
		if err != nil {
			clog.Errorf(ctx, "failed to read checksum of record at %d: %v", offset, err)
			break readLoop
		}
		expectedID := ^checksum
		if len(depsLog.paths) != int(expectedID) {
			clog.Errorf(ctx, "failed to match checksum %x -> %d != %d", checksum, expectedID, len(depsLog.paths))
		}
		depsLog.pathIdx[pathname] = len(depsLog.paths)
		depsLog.paths = append(depsLog.paths, pathname)
	}
	clog.Infof(ctx, "ninja deps %s => paths=%d, deps=%d", depsLog.fname, len(depsLog.paths), len(depsLog.deps))
	return depsLog, nil
}

// Close closes the deps log.
func (d *DepsLog) Close() error {
	if d == nil || d.w == nil {
		return nil
	}
	return d.w.Close()
}

func (d *DepsLog) update(ctx context.Context, outID int32, deps *depsRecord) {
	if int(outID) >= len(d.deps) {
		if int(outID) < cap(d.deps) {
			d.deps = d.deps[:outID+1]
		} else {
			// manually manage resizing, append would allocate ~1.5x what is needed
			// this is problematic because we need to handle lots of filenames
			newCap := ((outID + 100) / 100) * 100
			newDeps := make([]*depsRecord, outID+1, newCap)
			copy(newDeps, d.deps)
			d.deps = newDeps
		}
	}
	if log.V(3) {
		clog.Infof(ctx, "update deps out=%d deps=%v", outID, deps)
	}
	d.deps[outID] = deps
}

// Get returns deps log for the output.
func (d *DepsLog) Get(ctx context.Context, output string) ([]string, time.Time, error) {
	var mtime time.Time
	if d == nil {
		return nil, mtime, errors.New("no deps log")
	}
	output = filepath.ToSlash(output)
	d.mu.Lock()
	defer d.mu.Unlock()
	i, found := d.pathIdx[output]
	if !found {
		return nil, mtime, errors.New("not found")
	}
	if d.paths[i] != output {
		clog.Errorf(ctx, "inconsistent paths %s -> %d -> %s", output, i, d.paths[i])
		return nil, mtime, errors.New("inconsistent path in deps log")
	}
	deps, err := d.lookupDepRecord(ctx, i)
	if err != nil {
		clog.Warningf(ctx, "no deps for %s: %v", output, err)
		return nil, mtime, errors.New("no deps log entry")
	}
	return deps.inputs, time.Unix(int64(deps.mtime), 0), nil
}

func (d *DepsLog) lookupDepRecord(ctx context.Context, i int) (*depsRecord, error) {
	if i >= len(d.deps) {
		return nil, fmt.Errorf("index=%d (> %d)", i, len(d.deps))
	}
	deps := d.deps[i]
	if deps == nil {
		return nil, fmt.Errorf("index=%d nil entry", i)
	}
	return deps, nil
}

// Record records deps log for the output. This will write to disk.
// Returns whether any deps were updated.
func (d *DepsLog) Record(ctx context.Context, output string, mtime time.Time, deps []string) (bool, error) {
	if d == nil {
		return false, nil
	}
	output = filepath.ToSlash(output)
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.w == nil {
		var err error
		d.w, err = os.OpenFile(d.fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return false, err
		}
	}

	willUpdateDeps := false
	i, added := d.uniquePathIdx(output)
	if added {
		willUpdateDeps = true
		err := d.recordPath(ctx, i, output)
		if err != nil {
			return false, fmt.Errorf("failed to record for output %s: %v", output, err)
		}
	}
	var depIDs []int
	for _, dep := range deps {
		dep = filepath.ToSlash(dep)
		di, added := d.uniquePathIdx(dep)
		if added {
			willUpdateDeps = true
			err := d.recordPath(ctx, di, dep)
			if err != nil {
				return false, fmt.Errorf("failed to record for dep %s: %v", dep, err)
			}
		}
		depIDs = append(depIDs, di)
	}
	if !willUpdateDeps {
		dr, err := d.lookupDepRecord(ctx, i)
		if err != nil {
			willUpdateDeps = true
		} else {
			// Verify the stored record.
			// ignore mtime check?
			if len(depIDs) != len(dr.inputs) {
				willUpdateDeps = true
			} else {
				for i, di := range dr.inputs {
					if di != deps[i] {
						willUpdateDeps = true
					}
				}
			}
		}
	}
	if !willUpdateDeps {
		return false, nil
	}
	d.update(ctx, int32(i), &depsRecord{mtime: int32(mtime.Unix()), inputs: deps})
	err := d.recordDeps(ctx, i, mtime, depIDs)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *DepsLog) uniquePathIdx(path string) (int, bool) {
	i, found := d.pathIdx[path]
	if found {
		return i, false
	}
	d.paths = append(d.paths, path)
	i = len(d.paths) - 1
	d.pathIdx[path] = i
	return i, true
}

func createNewDepsLogFile(ctx context.Context, fname string) {
	f, err := os.Create(fname)
	if err != nil {
		clog.Warningf(ctx, "failed to create new deps log %s: %v", fname, err)
		return
	}
	_, err = f.Write([]byte(fileSignature))
	if err != nil {
		clog.Warningf(ctx, "failed to set file signature in %s: %v", fname, err)
	}
	err = binary.Write(f, binary.LittleEndian, int32(currentVersion))
	if err != nil {
		clog.Warningf(ctx, "failed to set version in %s: %v", fname, err)
	}
	err = f.Close()
	if err != nil {
		clog.Warningf(ctx, "failed to close %s: %v", fname, err)
	}
	clog.Infof(ctx, "created new deps log file: %s", fname)
}

func (d *DepsLog) recordPath(ctx context.Context, i int, path string) error {
	pathSize := len(path)
	padding := (4 - pathSize%4) % 4 // Pad path to 4 byte boundary.
	size := pathSize + padding + 4
	if size > maxRecordSize {
		return fmt.Errorf("too large record %d for %s", size, path)
	}
	// header: size
	// path record
	//  string name of the path
	//  up to 3 padding bytes to align on 4 byte boundaries
	//  one's complement of the expected index of the record
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, int32(size))
	buf.WriteString(path)
	buf.Write(make([]byte, padding))
	checksum := ^i
	binary.Write(&buf, binary.LittleEndian, int32(checksum))
	_, err := d.w.Write(buf.Bytes())
	return err
}

func (d *DepsLog) recordDeps(ctx context.Context, i int, mtime time.Time, inputs []int) error {
	size := 4 + 4 + 4*len(inputs)
	if size > maxRecordSize {
		return fmt.Errorf("too large record %d for %s", size, d.paths[i])
	}
	// header: size, high bit set.
	// array of 4-byte integers
	//   output path id
	//   output path mtime
	//   input path id
	header := uint32(size) | (1 << 31)
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, int32(header))
	binary.Write(&buf, binary.LittleEndian, int32(i))
	binary.Write(&buf, binary.LittleEndian, int32(mtime.Unix()))
	for _, id := range inputs {
		binary.Write(&buf, binary.LittleEndian, int32(id))
	}
	_, err := d.w.Write(buf.Bytes())
	return err
}
