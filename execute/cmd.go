// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package execute runs commands.
package execute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/types/known/durationpb"

	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/trace"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree"
)

// Executor is an interface to run the cmd.
type Executor interface {
	Run(ctx context.Context, cmd *Cmd) error
}

// FileTrace is the results of file trace of the cmd.
// The paths are relative to ExecRoot of the cmd.
type FileTrace struct {
	Inputs  []string
	Outputs []string
}

// REProxyConfig specifies configuration options for using reproxy.
type REProxyConfig struct {
	CanonicalizeWorkingDir bool              `json:"canonicalize_working_dir,omitempty"`
	DownloadOutputs        bool              `json:"download_outputs,omitempty"`
	PreserveSymlinks       bool              `json:"preserve_symlinks,omitempty"`
	ExecStrategy           string            `json:"exec_strategy,omitempty"`
	ExecTimeout            string            `json:"exec_timeout,omitempty"`     // duration format
	ReclientTimeout        string            `json:"reclient_timeout,omitempty"` // duration format
	Inputs                 []string          `json:"inputs,omitempty"`
	ToolchainInputs        []string          `json:"toolchain_inputs,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	Platform               map[string]string `json:"platform,omitempty"`
	RemoteWrapper          string            `json:"remote_wrapper,omitempty"`
	ServerAddress          string            `json:"server_address,omitempty"`
}

// Copy returns a deep copy of *REProxyConfig which can be safely mutated.
// Will return nil if given *REProxyConfig is nil.
func (c *REProxyConfig) Copy() *REProxyConfig {
	if c == nil {
		return nil
	}
	copy := *c
	copy.Labels = make(map[string]string, len(c.Labels))
	for k := range c.Labels {
		copy.Labels[k] = c.Labels[k]
	}
	copy.Platform = make(map[string]string, len(c.Platform))
	for k := range c.Platform {
		copy.Platform[k] = c.Platform[k]
	}
	return &copy
}

// Cmd includes all the information required to run a build command.
type Cmd struct {
	// ID is used as a unique identifier for this action in logs and tracing.
	// It does not have to be human-readable, so using a UUID is fine.
	ID string

	// Desc is a short, human-readable identifier that is shown to the user when referencing this action in the UI or a log file.
	// Example: "CXX hello.o"
	Desc string

	// ActionName is the name of the rule that generated this action.
	// Example: "cxx" or "link"
	ActionName string

	// Args holds command line arguments.
	Args []string

	// Env specifies the environment of the process.
	Env []string

	// RSPFile is the filename of the response file for the cmd.
	// If set,  Siso will write the RSPFileContent to the file before executing the action, and delete the file after executing the cmd successfully.
	RSPFile string

	// RSPFileContent is the content of the response file for the cmd.
	// The bindings are already expanded.
	RSPFileContent []byte

	// CmdHash is a hash of the command line, which is used to check for changes in the command line since it was last executed.
	CmdHash []byte

	// ExecRoot is an exec root directory of the cmd.
	ExecRoot string

	// Dir specifies the working directory of the cmd,
	// relative to ExecRoot.
	Dir string

	// Inputs are input files of the cmd, relative to ExecRoot.
	// They may be overridden by deps inputs.
	Inputs []string

	// ToolInputs are tool input files of the cmd, relative to ExecRoot.
	// They are specified by the siso config, not overridden by deps.
	// (or inputs would be deps + tool inputs).
	// These are expected to be toolchain input files, not by specified
	// by build deps, nor in deps log.
	ToolInputs []string

	// TreeInputs are precomputed subtree inputs of the cmd.
	TreeInputs []merkletree.TreeEntry

	// If UseSystemInputs is true, inputs may include system includes,
	// but it won't be included in remote exec request and expect such
	// files exist in platform container image.
	UseSystemInput bool

	// Outputs are output files of the cmd, relative to ExecRoot.
	Outputs []string

	// Deps specifies deps type of the cmd, "gcc", "msvc".
	Deps string

	// Depfile specifies a filename for dep info, relative to ExecRoot.
	Depfile string

	// If Restat is true
	// - output files may be used only for inputs
	// - no need to update mtime if content is not changed.
	Restat bool

	// Pure indicates whether the cmd is pure.
	// This is analogue to pure function.
	// For example, a cmd is pure when the inputs/outputs of the cmd are fully specified,
	// and it doesn't access other files during execution.
	// A pure cmd can execute remotely and the outputs can be safely cacheable.
	Pure bool

	// SkipCacheLookup specifies it won't lookup cache in remote execution.
	SkipCacheLookup bool

	// HashFS is a hash fs that the cmd runs on.
	HashFS *hashfs.HashFS

	// Platform is a platform properties for remote execution.
	// e.g. OSFamily: {Linux, Windows}
	Platform map[string]string

	// RemoteWrapper is a wrapper command when the cmd runs on remote execution backend.
	// It can be used to specify a wrapper command/script that exist on the worker.
	RemoteWrapper string

	// RemoteCommand is an argv[0] when the cmd runs on remote execution backend, if not empty.
	// e.g. "python3" for python actions sent from Windows host to Linux worker.
	RemoteCommand string

	// RemoteInputs are the substitute files for remote execution.
	// The key is the filename used in remote execution.
	// The value is the filename on local disk.
	// The file names are relative to ExecRoot.
	RemoteInputs map[string]string

	// REProxyConfig specifies configuration options for using reproxy.
	// If using reproxy, this config takes precedence over options in this struct.
	REProxyConfig *REProxyConfig

	// CanonicalizeDir specifies whether remote execution will canonicalize
	// working directory or not.
	CanonicalizeDir bool

	// DoNotCache specifies whether it won't update cache in remote execution.
	DoNotCache bool

	// Timeout specifies timeout of the cmd.
	Timeout time.Duration

	// ActionSalt is arbitrary bytes used for cache salt.
	ActionSalt []byte

	// FileTrace is a FileTrace info if enabled.
	FileTrace *FileTrace

	// Console indicates the command attaches stdin/stdout/stderr when
	// running.  localexec only.
	Console bool

	stdoutBuffer, stderrBuffer *bytes.Buffer

	actionDigest digest.Digest

	actionResult *rpb.ActionResult
	// actionResult is cached result if cachedResult is true.
	cachedResult bool

	// reproxy remote fallback
	remoteFallbackResult *rpb.ActionResult
}

// String returns an ID of the cmd.
func (c *Cmd) String() string {
	return c.ID
}

// AllInputs returns all inputs of the cmd.
func (c *Cmd) AllInputs() []string {
	if c.RSPFile == "" {
		return c.Inputs
	}
	inputs := make([]string, len(c.Inputs)+1)
	copy(inputs, c.Inputs)
	inputs[len(inputs)-1] = c.RSPFile
	return inputs
}

// AllOutputs returns all outputs of the cmd.
func (c *Cmd) AllOutputs() []string {
	if c.Depfile == "" {
		return c.Outputs
	}
	outputs := make([]string, len(c.Outputs)+1)
	copy(outputs, c.Outputs)
	outputs[len(outputs)-1] = c.Depfile
	return outputs
}

// RemoteArgs returns arguments to the remote command.
// The original args are adjusted with RemoteWrapper, RemoteCommand, Platform.
func (c *Cmd) RemoteArgs() ([]string, error) {
	args := c.Args
	if len(args) == 0 {
		return nil, errors.New("0 args")
	}
	// Cross-compile Windows builds on Linux workers.
	if runtime.GOOS == "windows" && c.Platform["OSFamily"] != "Windows" {
		args[0] = filepath.ToSlash(args[0])
	}
	if c.RemoteWrapper != "" {
		args = append([]string{c.RemoteWrapper}, args...)
	}
	if c.RemoteCommand != "" {
		// Replace the first args. But don't modify the Cmd.Args for fallback.
		args = append([]string{c.RemoteCommand}, args[1:]...)
	}
	return args, nil
}

// StdoutWriter returns a writer set for stdout.
func (c *Cmd) StdoutWriter() *bytes.Buffer {
	if c.stdoutBuffer == nil {
		c.stdoutBuffer = new(bytes.Buffer)
	}
	c.stdoutBuffer.Reset()
	return c.stdoutBuffer
}

// StderrWriter returns a writer set for stderr.
func (c *Cmd) StderrWriter() *bytes.Buffer {
	if c.stderrBuffer == nil {
		c.stderrBuffer = new(bytes.Buffer)
	}
	c.stderrBuffer.Reset()
	return c.stderrBuffer
}

// Stdout returns stdout output of the cmd.
func (c *Cmd) Stdout() []byte {
	if c.stdoutBuffer == nil {
		return nil
	}
	return c.stdoutBuffer.Bytes()
}

// Stderr returns stderr output of the cmd.
// Since RBE merges stderr into stdout, we won't get stderr for remote actions. b/149501385
// Therefore, we need to be careful how we use stdout/stderr for now.
// For example, if we use /showIncludes to stderr, it will be on stdout from a remote action.
func (c *Cmd) Stderr() []byte {
	if c.stderrBuffer == nil {
		return nil
	}
	return c.stderrBuffer.Bytes()
}

// ActionDigest returns action digest of the cmd.
func (c *Cmd) ActionDigest() digest.Digest {
	return c.actionDigest
}

// SetActionDigest sets action digest.
// This is used to set the digest provided by Reproxy.
func (c *Cmd) SetActionDigest(d digest.Digest) {
	c.actionDigest = d
}

// Digest computes action digest of the cmd.
// If ds is nil, then it will reuse the previous calculated digest if any.
// TODO(b/267576561): Integrate with Cloud Trace.
func (c *Cmd) Digest(ctx context.Context, ds *digest.Store) (digest.Digest, error) {
	if !c.Pure {
		return digest.Digest{}, fmt.Errorf("unable to create digest for impure cmd %s", c.ID)
	}
	if c.HashFS == nil {
		return digest.Digest{}, fmt.Errorf("unable to get the input root for %s: missing HashFS", c)

	}
	ents, err := c.inputTree(ctx)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to get input tree for %s: %w", c, err)
	}

	if c.CanonicalizeDir {
		ents = c.canonicalizeEntries(ctx, ents)
	}

	inputRootDigest, err := treeDigest(ctx, c.TreeInputs, ents, ds)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to get input root for %s: %w", c, err)
	}
	clog.Infof(ctx, "inputRoot: %s digests=%d", inputRootDigest, ds.Size())
	commandDigest, err := c.commandDigest(ctx, ds)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to build command for %s: %w", c, err)
	}
	clog.Infof(ctx, "command: %s", commandDigest)

	var timeout *durationpb.Duration
	if c.Timeout > 0 {
		// Set Timeout*2 to expect cache hit for long command.
		// but prevent from keeping RBE worker busy.
		timeout = durationpb.New(c.Timeout * 2)
	}

	action, err := digest.FromProtoMessage(&rpb.Action{
		CommandDigest:   commandDigest.Proto(),
		InputRootDigest: inputRootDigest.Proto(),
		Timeout:         timeout,
		DoNotCache:      c.DoNotCache,
		Salt:            c.ActionSalt,
		Platform:        c.remoteExecutionPlatform(),
	})
	if err != nil {
		return digest.Digest{}, fmt.Errorf("failed to build action for %s: %w", c, err)
	}
	if ds != nil {
		ds.Set(action)
	}
	c.actionDigest = action.Digest()
	clog.Infof(ctx, "action: %s", c.actionDigest)
	return c.actionDigest, nil
}

// inputTree returns Merkle tree entries for the cmd.
func (c *Cmd) inputTree(ctx context.Context) ([]merkletree.Entry, error) {
	inputs := c.AllInputs()

	if c.UseSystemInput {
		var newInputs []string
		for _, input := range inputs {
			if !filepath.IsLocal(input) {
				continue
			}
			newInputs = append(newInputs, input)
		}
		clog.Infof(ctx, "drop %d system inputs -> %d", len(inputs)-len(newInputs), len(newInputs))
		inputs = newInputs
	}

	if log.V(1) {
		clog.Infof(ctx, "tree @%s %s", c.ExecRoot, inputs)
	}
	ents, err := c.HashFS.Entries(ctx, c.ExecRoot, inputs)
	if err != nil {
		return nil, err
	}

	if len(c.RemoteInputs) == 0 {
		return ents, nil
	}
	if log.V(1) {
		clog.Infof(ctx, "remote tree @%s %s", c.ExecRoot, c.RemoteInputs)
	}

	// Construct a reverse map from local path to remote paths.
	// Note that multiple remote inputs may use the same local input.
	// Also, make a list of local filepaths to retrieve entries from the HashFS.
	revm := map[string][]string{}
	reins := make([]string, 0, len(c.RemoteInputs))
	for r, l := range c.RemoteInputs {
		reins = append(reins, l)
		revm[l] = append(revm[l], r)
	}

	// Retrieve Merkle tree entries from HashFS.
	sort.Strings(reins)
	reents, err := c.HashFS.Entries(ctx, c.ExecRoot, reins)
	if err != nil {
		return nil, err
	}

	// Convert local paths to remote paths.
	remap := map[string]merkletree.Entry{}
	for _, e := range reents {
		for _, rname := range revm[e.Name] {
			e.Name = rname
			remap[rname] = e
		}
	}

	// Replace local entries with the remote entries.
	for i, e := range ents {
		re, ok := remap[e.Name]
		if ok {
			ents[i] = re
			delete(remap, e.Name)
		}
	}

	// Append the remaining remote entries.
	for _, re := range remap {
		ents = append(ents, re)
	}

	sort.Slice(ents, func(i, j int) bool {
		return ents[i].Name < ents[j].Name
	})
	return ents, nil
}

// treeDigest returns a digest for the Merkle tree entries.
func treeDigest(ctx context.Context, subtrees []merkletree.TreeEntry, entries []merkletree.Entry, ds *digest.Store) (digest.Digest, error) {
	t := merkletree.New(ds)
	for _, subtree := range subtrees {
		if log.V(2) {
			clog.Infof(ctx, "input subtree: %#v", subtree)
		}
		err := t.SetTree(subtree)
		if errors.Is(err, merkletree.ErrPrecomputedSubTree) {
			// probably wrong TreeInputs are set.
			// assume upper subtree covers lower subtree,
			// so ignore ErrPrecomputedSubTree here.
			clog.Warningf(ctx, "ignore subtree %v: %v", subtree, err)
			continue
		}
		if err != nil {
			return digest.Digest{}, err
		}
	}
	for _, ent := range entries {
		if log.V(2) {
			clog.Infof(ctx, "input entry: %#v", ent)
		}
		err := t.Set(ent)
		if errors.Is(err, merkletree.ErrPrecomputedSubTree) {
			// wrong config or deps uses files in subtree.
			// assume subtree contains the file,
			// so ignore ErrPrecomputedSubTree here.
			if log.V(1) {
				clog.Warningf(ctx, "ignore entry in subtree %v: %v", ent, err)
			}
			continue
		}
		if err != nil {
			return digest.Digest{}, err
		}
	}

	d, err := t.Build(ctx)
	if err != nil {
		return digest.Digest{}, err
	}
	return d, nil
}

// canonicalizeEntries canonicalizes working dirs in the entries.
func (c *Cmd) canonicalizeEntries(ctx context.Context, entries []merkletree.Entry) []merkletree.Entry {
	cdir := c.canonicalDir()
	if cdir == "" {
		return entries
	}
	if log.V(1) {
		clog.Infof(ctx, "canonicalize dir: %s -> %s", c.Dir, cdir)
	}
	for i := range entries {
		e := &entries[i]
		e.Name = canonicalizeDir(e.Name, c.Dir, cdir)
	}
	return entries
}

// canonicalDir computes a canonical dir of the working directory.
func (c *Cmd) canonicalDir() string {
	if c.Dir == "" || c.Dir == "." {
		return ""
	}
	n := len(strings.Split(filepath.ToSlash(c.Dir), "/"))
	elems := []string{"out"}
	for i := 1; i < n; i++ {
		elems = append(elems, "x")
	}
	return filepath.Join(elems...)
}

func canonicalizeDir(fname, dir, cdir string) string {
	if dir == cdir {
		return fname
	}
	if fname == dir {
		return cdir
	}
	for _, prefix := range []string{dir + "/", dir + `\`} {
		if f, ok := strings.CutPrefix(fname, prefix); ok {
			return filepath.Join(cdir, f)
		}
	}
	return fname
}

// remoteExecutionPlatform constructs a Remote Execution Platform properties from the platform properties.
func (c *Cmd) remoteExecutionPlatform() *rpb.Platform {
	platform := &rpb.Platform{}
	for k, v := range c.Platform {
		platform.Properties = append(platform.Properties, &rpb.Platform_Property{
			Name:  k,
			Value: v,
		})
	}
	sort.Slice(platform.Properties, func(i, j int) bool {
		return platform.Properties[i].Name < platform.Properties[j].Name
	})
	return platform
}

// commandDigest constructs the digest of the command line.
func (c *Cmd) commandDigest(ctx context.Context, ds *digest.Store) (digest.Digest, error) {
	outputs := c.AllOutputs()
	outs := make([]string, 0, len(outputs))
	for _, out := range outputs {
		rout, err := filepath.Rel(c.Dir, out)
		if err != nil {
			clog.Warningf(ctx, "failed to get rel %s,%s: %v", c.Dir, out, err)
			rout = out
		}
		outs = append(outs, filepath.ToSlash(rout))
	}
	args, err := c.RemoteArgs()
	if err != nil {
		return digest.Digest{}, err
	}
	dir := c.Dir
	if c.CanonicalizeDir {
		dir = c.canonicalDir()
	}
	command := &rpb.Command{
		Arguments:        args,
		WorkingDirectory: filepath.ToSlash(dir),
		// TODO(b/273151098): `OutputFiles` is deprecated. should use `OutputPaths` instead.
		// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L592
		OutputFiles: outs,
		// TODO(b/273152496): `Platform` in `Command` is deprecated. should specify it in `Action`.
		// https://github.com/bazelbuild/remote-apis/blob/55153ba61dcf6277849562a30bca9fa3906ad9a0/build/bazel/remote/execution/v2/remote_execution.proto#L661-L664
		Platform: c.remoteExecutionPlatform(), // deprecated?
	}
	for _, env := range c.Env {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		command.EnvironmentVariables = append(command.EnvironmentVariables, &rpb.Command_EnvironmentVariable{
			Name:  k,
			Value: v,
		})
	}
	sort.Slice(command.EnvironmentVariables, func(i, j int) bool {
		return command.EnvironmentVariables[i].Name < command.EnvironmentVariables[j].Name
	})
	data, err := digest.FromProtoMessage(command)
	if err != nil {
		return digest.Digest{}, err
	}
	if ds != nil {
		ds.Set(data)
	}
	return data.Digest(), nil
}

// SetActionResult sets action result to the cmd.
func (c *Cmd) SetActionResult(result *rpb.ActionResult, cached bool) {
	c.actionResult = result
	c.cachedResult = cached
}

// ActionResult returns the action result of the cmd.
func (c *Cmd) ActionResult() (*rpb.ActionResult, bool) {
	return c.actionResult, c.cachedResult
}

// SetRemoteFallbackResult sets remote action failed result for reproxy.
func (c *Cmd) SetRemoteFallbackResult(result *rpb.ActionResult) {
	c.remoteFallbackResult = result
}

// RemoteFallbackResult returns the remote action failed result of the cmd for reproxy.
func (c *Cmd) RemoteFallbackResult() *rpb.ActionResult {
	return c.remoteFallbackResult
}

// entriesFromResult returns output file entries and depfile entries for the cmd and result.
// if depfile is in output, its entry is in entries, not depEntries to have
// cmdhash in the entry.
func (c *Cmd) entriesFromResult(ctx context.Context, ds hashfs.DataSource, result *rpb.ActionResult) (entries, depEntries []merkletree.Entry) {
	depfile := c.Depfile
	for _, out := range c.Outputs {
		if out == depfile {
			depfile = ""
			break
		}
	}

	for _, f := range result.GetOutputFiles() {
		if f.Digest == nil {
			continue
		}
		fname := filepath.ToSlash(filepath.Join(c.Dir, f.Path))
		d := digest.FromProto(f.Digest)
		if fname == depfile {
			depEntries = append(depEntries, merkletree.Entry{
				Name:         fname,
				Data:         digest.NewData(ds.Source(d, fname), d),
				IsExecutable: f.IsExecutable,
			})
			continue
		}
		entries = append(entries, merkletree.Entry{
			Name:         fname,
			Data:         digest.NewData(ds.Source(d, fname), d),
			IsExecutable: f.IsExecutable,
		})
	}
	for _, s := range result.GetOutputSymlinks() {
		if s.Target == "" {
			continue
		}
		fname := filepath.ToSlash(filepath.Join(c.Dir, s.Path))
		entries = append(entries, merkletree.Entry{
			Name:   fname,
			Target: s.Target,
		})
	}
	for _, d := range result.GetOutputDirectories() {
		// It just needs to add the directories here because it assumes that they have already been expanded by ninja State.
		dname := filepath.ToSlash(filepath.Join(c.Dir, d.Path))
		entries = append(entries, merkletree.Entry{
			Name: dname,
		})
	}
	return entries, depEntries
}

func hashfsUpdate(ctx context.Context, hfs *hashfs.HashFS, execRoot string, entries []merkletree.Entry, mtime time.Time, cmdhash []byte, action digest.Digest) error {
	ents := make([]hashfs.UpdateEntry, 0, len(entries))
	for _, ent := range entries {
		mode := fs.FileMode(0644)
		switch {
		case !ent.Data.IsZero():
			if ent.IsExecutable {
				mode |= 0111
			}
		case ent.Target != "":
			mode = 0644 | fs.ModeSymlink
		default: // directory
			mode = 0755 | fs.ModeDir
		}

		ents = append(ents, hashfs.UpdateEntry{
			Entry:       ent,
			Mode:        mode,
			ModTime:     mtime,
			CmdHash:     cmdhash,
			Action:      action,
			UpdatedTime: mtime,
			IsChanged:   true,
		})
	}
	return hfs.Update(ctx, execRoot, ents)
}

// RecordOutputs records cmd's outputs from action result in hashfs.
func (c *Cmd) RecordOutputs(ctx context.Context, ds hashfs.DataSource, now time.Time) error {
	entries, depEntries := c.entriesFromResult(ctx, ds, c.actionResult)
	clog.Infof(ctx, "output entries %d+%d", len(entries), len(depEntries))
	err := hashfsUpdate(ctx, c.HashFS, c.ExecRoot, entries, now, c.CmdHash, c.actionDigest)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from remote: %w", err)
	}
	if len(depEntries) == 0 {
		return nil
	}
	err = hashfsUpdate(ctx, c.HashFS, c.ExecRoot, depEntries, now, nil, c.actionDigest)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from remote[depfile]: %w", err)
	}
	return nil
}

func hashfsUpdateFromLocal(ctx context.Context, hfs *hashfs.HashFS, root string, inputs []string, restat bool, updatedTime time.Time, cmdhash []byte) error {
	ctx, span := trace.NewSpan(ctx, "fs-update-local")
	defer span.Close(nil)

	m := make(map[string]hashfs.UpdateEntry)
	if restat {
		// retrieve entries stored in hashfs.
		// TODO: record this before local execution.
		entries := hfs.RetrieveUpdateEntries(ctx, root, inputs)
		for _, ent := range entries {
			m[ent.Entry.Name] = ent
		}
	}

	// forget and retrieve entries from local disk.
	hfs.Forget(ctx, root, inputs)
	entries := hfs.RetrieveUpdateEntries(ctx, root, inputs)

	// Set cmdhash, updatedTime.
	// also isChanged=true if entry has been changed.
	for i, ent := range entries {
		ent.CmdHash = cmdhash
		ent.UpdatedTime = updatedTime
		ent.IsLocal = true
		pent := m[ent.Entry.Name]
		if !restat {
			ent.ModTime = updatedTime
			ent.IsChanged = true
		} else if !pent.ModTime.Equal(ent.ModTime) {
			// TODO: check digest too?
			ent.IsChanged = true
		}
		entries[i] = ent
	}
	// store in hashfs.
	return hfs.Update(ctx, root, entries)
}

// RecordOutputsFromLocal records cmd's outputs from local disk in hashfs.
func (c *Cmd) RecordOutputsFromLocal(ctx context.Context, now time.Time) error {
	if c.Depfile != "" {
		err := hashfsUpdateFromLocal(ctx, c.HashFS, c.ExecRoot, []string{c.Depfile}, c.Restat, now, nil)
		if err != nil {
			return fmt.Errorf("failed to update hashfs from local[depfile]: %w", err)
		}
	}
	err := hashfsUpdateFromLocal(ctx, c.HashFS, c.ExecRoot, c.Outputs, c.Restat, now, c.CmdHash)
	if err != nil {
		return fmt.Errorf("failed to update hashfs from local: %w", err)
	}
	return nil
}

// ResultFromEntries updates result from entries.
func ResultFromEntries(result *rpb.ActionResult, entries []merkletree.Entry) {
	for _, ent := range entries {
		switch {
		case ent.IsSymlink():
			result.OutputSymlinks = append(result.OutputSymlinks, &rpb.OutputSymlink{
				Path:   ent.Name,
				Target: ent.Target,
			})
		case ent.IsDir():
			result.OutputDirectories = append(result.OutputDirectories, &rpb.OutputDirectory{
				Path: ent.Name,
				// TODO(b/275448031): calculate tree digest from the entry.
				TreeDigest: digest.Empty.Proto(),
			})
		default:
			result.OutputFiles = append(result.OutputFiles, &rpb.OutputFile{
				Path:         ent.Name,
				Digest:       ent.Data.Digest().Proto(),
				IsExecutable: ent.IsExecutable,
			})
		}
	}
}

// ExitError is an error of cmd exit.
type ExitError struct {
	ExitCode int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit=%d", e.ExitCode)
}
