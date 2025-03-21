// Copyright 2023 The Chromium Authors
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
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/glog"
)

// DepsLog is an in-memory representation of ninja's depslog.
// It supports creating new depslog files and reading existing depslog files,
// as well as adding new records to open depslog files.
// Format:
// https://github.com/ninja-build/ninja/blob/87111bff382655075f2577c591745a335f0103c7/src/deps_log.h
type DepsLog struct {
	fname string

	// read-only data for Get.
	rPaths   []string
	rPathIdx map[string]int
	rDeps    []*depsRecord

	mu      sync.Mutex
	paths   []string
	pathIdx map[string]int
	deps    []*depsRecord

	w *os.File

	needsRecompact bool
}

const fileSignature = "# ninjadeps\n"
const currentVersion = 4
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
	mtime  int64
	inputs []string
}

func verifySignature(ctx context.Context, f io.Reader) error {
	buf := make([]byte, len(fileSignature))
	n, err := f.Read(buf)
	if glog.V(3) {
		glog.Infof("signature=%q: %d %v", buf, n, err)
	}
	if err != nil || n != len(buf) {
		return fmt.Errorf("failed to read file signature=%d: %w", n, err)
	}
	if !bytes.Equal(buf, []byte(fileSignature)) {
		return fmt.Errorf("wrong signature %q", buf)
	}
	return nil
}

func verifyVersion(ctx context.Context, f io.Reader) error {
	var ver int32
	err := binary.Read(f, binary.LittleEndian, &ver)
	if glog.V(3) {
		glog.Infof("version=%d: %v", ver, err)
	}
	if err != nil {
		return fmt.Errorf("failed to read version: %w", err)
	}
	if ver != currentVersion {
		return fmt.Errorf("wrong version %d", ver)
	}
	return nil
}

func readRecordHeader(ctx context.Context, f io.Reader) (recordHeader, error) {
	var header recordHeader
	err := binary.Read(f, binary.LittleEndian, &header)
	if glog.V(3) {
		glog.Infof("header=0x%0x: %v", header, err)
	}
	if err != nil {
		return -1, err
	}
	return header, nil
}

func readDepsRecord(ctx context.Context, buf []byte, size int, depsLogPaths []string) (int32, *depsRecord, error) {
	// dependency record
	// array of 4-byte integers
	//   output path id
	//   output path mtime_lo
	//   output path mtime_hi
	//   input path id, ...
	rec := make([]int32, size/4)
	err := binary.Read(bytes.NewReader(buf[:size]), binary.LittleEndian, rec)
	if glog.V(3) {
		glog.Infof("deps record=%v: %v", rec, err)
	}
	if err != nil {
		return -1, nil, err
	}
	outID := rec[0]
	mtimeLo := rec[1]
	mtimeHi := rec[2]
	mtime := int64(uint32(mtimeHi))<<32 | int64(uint32(mtimeLo))
	rec = rec[3:]
	deps := &depsRecord{mtime: mtime, inputs: make([]string, 0, len(rec))}
	for _, id := range rec {
		if int(id) < 0 || int(id) >= len(depsLogPaths) {
			glog.Warningf("bad path id=%d (depsLog.paths=%d)", id, len(depsLogPaths))
			return -1, nil, nil
		}
		deps.inputs = append(deps.inputs, depsLogPaths[id])
	}

	return outID, deps, nil
}

func readPathRecord(ctx context.Context, buf []byte, size int, numDepsLogPaths int) (string, error) {
	// path record
	//  string name of the path
	//  up to 3 padding bytes to align on 4 byte boundaries
	//  one's complement of the expected index of the record
	//  checksum (4 bytes)
	pathSize := size - 4
	for range 3 {
		if pathSize-1 < 0 || pathSize-1 >= len(buf) {
			return "", fmt.Errorf("path size is corrupted? size=%d buf_size=%d", size, len(buf))
		}
		if buf[pathSize-1] == 0 {
			pathSize--
		}
	}
	pathname := string(buf[:pathSize])
	if glog.V(3) {
		glog.Infof("path record %q %d", pathname, numDepsLogPaths)
	}

	var checksum int32
	err := binary.Read(bytes.NewReader(buf[size-4:size]), binary.LittleEndian, &checksum)
	if glog.V(3) {
		glog.Infof("checksum %x: %v", checksum, err)
	}
	if err != nil {
		return "", err
	}
	expectedID := ^checksum
	if numDepsLogPaths != int(expectedID) {
		return "", fmt.Errorf("failed to match checksum %x -> %d != %d", checksum, expectedID, numDepsLogPaths)
	}
	return pathname, nil
}

// NewDepsLog reads or creates a new deps log.
// If there are read errors, returns a truncated deps log.
func NewDepsLog(ctx context.Context, fname string) (*DepsLog, error) {
	if fname == "" {
		return nil, errors.New("no ninja_deps")
	}
	depsLog := &DepsLog{
		fname:    fname,
		rPathIdx: make(map[string]int),
		pathIdx:  make(map[string]int),
	}
	fbuf, err := os.ReadFile(fname)
	if os.IsNotExist(err) {
		glog.Infof("ninja_deps %s doesn't exist: %v", fname, err)
		createNewDepsLogFile(ctx, fname)
		return depsLog, nil
	}
	if err != nil {
		return nil, err
	}
	f := bytes.NewReader(fbuf)
	if err = verifySignature(ctx, f); err != nil {
		glog.Warningf("ninja_deps %s error: %v", fname, err)
		createNewDepsLogFile(ctx, fname)
		return depsLog, nil
	}
	if err = verifyVersion(ctx, f); err != nil {
		glog.Warningf("ninja_deps %s error: %v", fname, err)
		createNewDepsLogFile(ctx, fname)
		return depsLog, nil
	}
	var offset int64
	// TODO(ukai): this reflects original ninja impl, may not be optimal
	buf := make([]byte, maxRecordSize+1)
	// Perform read loop. Truncates with current state of deps log if error
	// encountered while parsing (i.e. broken input should never cause error)
	totalRecords := 0
	uniqueRecords := 0
	broken := false
readLoop:
	for {
		offset, err = f.Seek(0, os.SEEK_CUR)
		if glog.V(3) {
			glog.Infof("offset=%d: %v", offset, err)
		}
		if err != nil {
			glog.Errorf("failed to get offset: %v", err)
			broken = true
			break readLoop
		}
		header, err := readRecordHeader(ctx, f)
		if err != nil {
			if err != io.EOF {
				glog.Errorf("failed to read header at %d: %v", offset, err)
				broken = true
			}
			break readLoop
		}
		size := header.RecordSize()
		if size > maxRecordSize {
			glog.Errorf("too large record %d at %d", size, offset)
			broken = true
			break readLoop
		}
		n, err := f.Read(buf[:size])
		if err != nil || n != size {
			glog.Errorf("failed to read record %d at %d: n=%d, %v", size, offset, n, err)
			broken = true
			break readLoop
		}
		if header.IsDepsRecord() {
			outID, deps, err := readDepsRecord(ctx, buf, size, depsLog.paths)
			if err != nil {
				glog.Errorf("failed to parse deps record at %d: %v", offset, err)
				broken = true
				break readLoop
			}
			// don't update if deps record is invalid, but allow continuation
			if outID != -1 {
				totalRecords++
				if !depsLog.update(ctx, outID, deps) {
					uniqueRecords++
				}
			}
		} else {
			pathname, err := readPathRecord(ctx, buf, size, len(depsLog.paths))
			if err != nil {
				glog.Errorf("failed to parse path record at %d: %v", offset, err)
				broken = true
				break readLoop
			}
			depsLog.pathIdx[pathname] = len(depsLog.paths)
			depsLog.paths = append(depsLog.paths, pathname)
		}
	}
	// need recompact the log if there are too many dead records.
	// https://github.com/ninja-build/ninja/blob/36843d387cb0621c1a288179af223d4f1410be73/src/deps_log.cc#L280
	const minCompactionEntryCount = 1000
	const compactionRatio = 3
	depsLog.needsRecompact = broken || (totalRecords > minCompactionEntryCount && totalRecords > uniqueRecords*compactionRatio) || totalRecords == 0

	glog.Infof("ninja deps %s => paths=%d, deps=%d total=%d unique=%d recompact=%t (broken:%t)", depsLog.fname, len(depsLog.paths), len(depsLog.deps), totalRecords, uniqueRecords, depsLog.needsRecompact, broken)
	depsLog.rPaths = depsLog.paths
	maps.Copy(depsLog.rPathIdx, depsLog.pathIdx)
	depsLog.rDeps = depsLog.deps

	return depsLog, nil
}

// Reset resets deps log, so recorded entries is available for Get.
func (d *DepsLog) Reset() {
	d.mu.Lock()
	d.rPaths = d.paths
	maps.Copy(d.rPathIdx, d.pathIdx)
	d.rDeps = d.deps
	d.mu.Unlock()
}

// NeedsRecompact reports whether it needs recompact or not.
func (d *DepsLog) NeedsRecompact() bool {
	return d.needsRecompact
}

// Recompact recompacts deps log file, i.e.
// rewrites the known log entries, throwing away old data.
func (d *DepsLog) Recompact(ctx context.Context) error {
	glog.Infof(".siso_deps recompact")
	err := d.Close()
	if err != nil {
		return fmt.Errorf("failed to close before recompact: %w", err)
	}

	tempPath := d.fname + ".recompact"
	// openForWrite() opens for append.  Make sure it's not appending to a
	// left-over file from a previous recompaction attempt that crashed somehow.
	os.Remove(tempPath)
	createNewDepsLogFile(ctx, tempPath)
	nd := &DepsLog{
		fname:    tempPath,
		rPathIdx: make(map[string]int),
		pathIdx:  make(map[string]int),
	}
	err = nd.openForWrite(ctx)
	if err != nil {
		return err
	}

	// write out all deps again.
	for i := range len(d.rDeps) {
		deps := d.rDeps[i]
		if deps == nil {
			continue
		}
		// TODO: ignore if entry has deps= for now?
		out := d.rPaths[i]
		mtime := time.Unix(0, deps.mtime)
		_, err = nd.Record(ctx, out, mtime, deps.inputs)
		if err != nil {
			nd.Close()
			return fmt.Errorf("record in recompaction: %w", err)
		}
	}
	err = nd.Close()
	if err != nil {
		return fmt.Errorf("close recompacted deps log: %w", err)
	}

	err = os.Remove(d.fname)
	if err != nil {
		return fmt.Errorf("remove old deps log: %w", err)
	}
	err = os.Rename(nd.fname, d.fname)
	if err != nil {
		return fmt.Errorf("rename compacted deps log: %w", err)
	}

	d.mu.Lock()
	d.rPaths = nd.paths
	maps.Copy(d.rPathIdx, nd.pathIdx)
	d.rDeps = nd.deps

	d.paths = nd.paths
	d.pathIdx = nd.pathIdx
	d.deps = nd.deps
	d.mu.Unlock()

	d.needsRecompact = false
	return nil
}

// Close closes the deps log.
func (d *DepsLog) Close() error {
	if d == nil || d.w == nil {
		return nil
	}
	err := d.w.Close()
	d.w = nil
	return err
}

// update updates deps records of output identified by outID
// and returns true if updated, false if added.
func (d *DepsLog) update(ctx context.Context, outID int32, deps *depsRecord) bool {
	existed := int(outID) < len(d.deps)
	if !existed {
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
	if glog.V(3) {
		glog.Infof("update deps out=%d deps=%v", outID, deps)
	}
	d.deps[outID] = deps
	return existed
}

var ErrNoDepsLog = errors.New("deps not found")

// Get returns deps log for the output.
func (d *DepsLog) Get(ctx context.Context, output string) ([]string, time.Time, error) {
	var mtime time.Time
	if d == nil {
		return nil, mtime, errors.New("no deps log")
	}
	output = filepath.ToSlash(output)
	i, found := d.rPathIdx[output]
	if !found {
		return nil, mtime, ErrNoDepsLog
	}
	if i < 0 || i >= len(d.rPaths) {
		return nil, mtime, fmt.Errorf("no path entry for %s %d: %w", output, i, ErrNoDepsLog)
	}
	if d.rPaths[i] != output {
		glog.Errorf("inconsistent paths %s -> %d -> %s", output, i, d.rPaths[i])
		return nil, mtime, errors.New("inconsistent path in deps log")
	}
	if i >= len(d.rDeps) {
		return nil, mtime, fmt.Errorf("no deps log entry: %w", ErrNoDepsLog)
	}
	deps := d.rDeps[i]
	if deps == nil {
		return nil, mtime, fmt.Errorf("no deps log entry: %w", ErrNoDepsLog)
	}
	return deps.inputs, time.Unix(0, deps.mtime), nil
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

func (d *DepsLog) openForWrite(ctx context.Context) error {
	var err error
	d.w, err = os.OpenFile(d.fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	return err
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
		err := d.openForWrite(ctx)
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
			return false, fmt.Errorf("failed to record for output %s: %w", output, err)
		}
	}
	var depIDs []int
	for i, dep := range deps {
		dep = filepath.ToSlash(dep)
		di, added := d.uniquePathIdx(dep)
		deps[i] = d.paths[di]
		if added {
			willUpdateDeps = true
			err := d.recordPath(ctx, di, dep)
			if err != nil {
				return false, fmt.Errorf("failed to record for dep %s: %w", dep, err)
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
			if len(depIDs) != len(dr.inputs) {
				willUpdateDeps = true
			} else if mtime.UnixNano() != dr.mtime {
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
	d.update(ctx, int32(i), &depsRecord{mtime: mtime.UnixNano(), inputs: deps})
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
		glog.Warningf("failed to create new deps log %s: %v", fname, err)
		return
	}
	_, err = f.Write([]byte(fileSignature))
	if err != nil {
		glog.Warningf("failed to set file signature in %s: %v", fname, err)
	}
	err = binary.Write(f, binary.LittleEndian, int32(currentVersion))
	if err != nil {
		glog.Warningf("failed to set version in %s: %v", fname, err)
	}
	err = f.Close()
	if err != nil {
		glog.Warningf("failed to close %s: %v", fname, err)
	}
	glog.Infof("created new deps log file: %s", fname)
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
	size := 4 + 4 + 4 + 4*len(inputs)
	if size > maxRecordSize {
		return fmt.Errorf("too large record %d for %s", size, d.paths[i])
	}
	// header: size, high bit set.
	// array of 4-byte integers
	//   output path id
	//   output path mtime_lo
	//   output path mtime_hi
	//   input path id
	header := uint32(size) | (1 << 31)
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, int32(header))
	binary.Write(&buf, binary.LittleEndian, int32(i))
	t := mtime.UnixNano()
	mtimeLo := int32(t & 0xffffffff)
	binary.Write(&buf, binary.LittleEndian, mtimeLo)
	mtimeHi := int32(t >> 32)
	binary.Write(&buf, binary.LittleEndian, mtimeHi)
	for _, id := range inputs {
		binary.Write(&buf, binary.LittleEndian, int32(id))
	}
	_, err := d.w.Write(buf.Bytes())
	return err
}

// RecordedTargets returns a list of targets that have deps log.
func (d *DepsLog) RecordedTargets() []string {
	var targets []string
	for i, target := range d.rPaths {
		if i >= len(d.rDeps) {
			break
		}
		if d.rDeps[i] == nil {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}
