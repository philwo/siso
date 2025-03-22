// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package buildconfig provides build config for `siso ninja`.
package buildconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/charmbracelet/log"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"go.chromium.org/infra/build/siso/build"
	"go.chromium.org/infra/build/siso/build/metadata"
	"go.chromium.org/infra/build/siso/execute"
	"go.chromium.org/infra/build/siso/hashfs"
)

const configEntryPoint = "init"

// Config is a build config.
type Config struct {
	// Metadata contains key-value metadata for the build.
	Metadata metadata.Metadata

	// flags used to run the build.
	flags map[string]string

	// global variables loaded by the config.
	globals map[string]starlark.Value

	// handlers registered by the config.
	handlers *starlark.Dict

	// filegroups registered by the config.
	filegroups map[string]filegroupUpdater

	// filesystem cache used for handlers.
	fscache *fscache
}

// New returns new build config.
func New(ctx context.Context, fname string, flags map[string]string, repos map[string]fs.FS) (*Config, error) {
	metadata := metadata.New()
	if repos == nil {
		repos = map[string]fs.FS{}
	}
	repos["builtin"] = builtinStar
	if _, ok := repos[configRepo]; !ok {
		return nil, errors.New("config module is not set")
	}
	if _, ok := repos[configOverridesRepo]; !ok {
		repos[configOverridesRepo] = emptyFS{}
	}

	loader := &repoLoader{
		ctx:         ctx,
		repos:       repos,
		predeclared: builtinModule(),
	}
	log.Infof("enable starlark recursion")
	resolve.AllowRecursion = true

	thread := &starlark.Thread{
		Name: "load",
		Print: func(thread *starlark.Thread, msg string) {
			log.Infof("thread:%s %s", thread.Name, msg)
		},
		Load: loader.Load,
	}
	thread.SetLocal("modulename", fname)
	globals, err := loader.Load(thread, fname)
	if err != nil {
		log.Warnf("thread:%s failed to exec file %s: %v", thread.Name, fname, err)
		var eerr *starlark.EvalError
		if errors.As(err, &eerr) {
			log.Warnf("stacktrace:\n%s", eerr.Backtrace())
		}
		return nil, err
	}
	log.Infof("config: %s", globals)
	v, ok := globals[configEntryPoint]
	if !ok {
		return nil, fmt.Errorf("%s is not defined in %s", configEntryPoint, fname)
	}
	if _, ok := v.(starlark.Callable); !ok {
		return nil, fmt.Errorf("%s %T is not callable in %s", configEntryPoint, v.Type(), fname)
	}
	return &Config{
		Metadata: metadata,
		flags:    flags,
		globals:  globals,
		fscache: &fscache{
			m: make(map[string][]byte),
		},
	}, nil
}

// HandlerError is error of handler.
type HandlerError struct {
	entry string
	fn    starlark.Value
	err   *starlark.EvalError
}

func (e HandlerError) Error() string {
	if fn, ok := e.fn.(*starlark.Function); ok {
		return fmt.Sprintf("failed to run %s[%s:%s]: %v", e.entry, fn.Position(), fn.Name(), e.err)

	}
	return fmt.Sprintf("failed to run %s[%s]: %v", e.entry, e.fn, e.err)
}

func (e HandlerError) Backtrace() string {
	return e.err.CallStack.String()
}

func (e HandlerError) Unwrap() error {
	return e.err
}

// Init initializes config by running `init`.
func (cfg *Config) Init(ctx context.Context, hashFS *hashfs.HashFS, buildPath *build.Path) (string, error) {
	// Clear fscache to read updated contents after `gn gen`.
	cfg.fscache = &fscache{m: make(map[string][]byte)}

	fun, ok := cfg.globals[configEntryPoint]
	if !ok {
		return "", fmt.Errorf("no %s", configEntryPoint)
	}
	thread := &starlark.Thread{
		Name: configEntryPoint,
		Print: func(thread *starlark.Thread, msg string) {
			log.Infof("thread:%s %s", thread.Name, msg)
		},
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, fmt.Errorf("load is not allowed in init")
		},
	}

	hctx := starlarkstruct.FromStringDict(starlark.String("ctx"), map[string]starlark.Value{
		"actions":  starInitActions(cfg.Metadata),
		"metadata": starMetadata(cfg.Metadata),
		"flags":    starFlags(cfg.flags),
		// want "envs" ?
		"fs": starFS(ctx, hashFS.FileSystem(ctx, buildPath.ExecRoot), buildPath, cfg.fscache),
	})
	log.Infof("hctx: %v", hctx)
	ret, err := starlark.Call(thread, fun, []starlark.Value{hctx}, nil)
	if err != nil {
		log.Warnf("thread:%s failed to run %s: %v", thread.Name, configEntryPoint, err)
		var eerr *starlark.EvalError
		if errors.As(err, &eerr) {
			log.Warnf("stacktrace:\n%s", eerr.Backtrace())
			return "", HandlerError{entry: configEntryPoint, fn: fun, err: eerr}
		}
		return "", fmt.Errorf("failed to run %s: %w", configEntryPoint, err)
	}
	m, ok := ret.(*starlarkstruct.Module)
	if !ok {
		return "", fmt.Errorf("%s returned %s, want module", configEntryPoint, ret.Type())
	}
	h, err := m.Attr("handlers")
	if err != nil {
		return "", fmt.Errorf("no handlers in %v: %w", ret, err)
	}
	handlers, ok := h.(*starlark.Dict)
	if !ok {
		return "", fmt.Errorf("handlers %v, want dict", h)
	}
	cfg.handlers = handlers

	fg, err := m.Attr("filegroups")
	if err != nil {
		return "", fmt.Errorf("no filegroups in %v: %w", ret, err)
	}
	cfg.filegroups, err = parseFilegroups(fg)
	if err != nil {
		return "", fmt.Errorf("bad filegroups: %w", err)
	}

	stepConfig, err := m.Attr("step_config")
	if err != nil {
		return "", fmt.Errorf("no step_config in %v: %w", ret, err)
	}
	s, ok := starlark.AsString(stepConfig)
	if !ok {
		return "", fmt.Errorf("%s returned %s, want string", configEntryPoint, ret.Type())
	}
	return s, nil
}

// Func returns a function for the handler name.
func (cfg *Config) Func(handler string) (starlark.Value, bool) {
	if cfg.handlers == nil {
		log.Warnf("no handlers")
		return starlark.None, false
	}
	fun, ok, err := cfg.handlers.Get(starlark.String(handler))
	if !ok || err != nil {
		log.Warnf("no handler:%q ok:%t err:%v dict:%v keys:%v", handler, ok, err, cfg.handlers, cfg.handlers.Keys())
	}
	return fun, ok && err == nil
}

// Handle runs handler for the cmd.
func (cfg *Config) Handle(ctx context.Context, handler string, bpath *build.Path, cmd *execute.Cmd, expandedInputs func() []string) (err error) {
	fun, ok := cfg.Func(handler)
	if !ok {
		return fmt.Errorf("no handler:%q for %s", handler, cmd)
	}
	started := time.Now()
	defer func() {
		log.Infof("handle:%s %s", handler, time.Since(started))
	}()
	thread := &starlark.Thread{
		Name: "handler:" + handler,
		Print: func(thread *starlark.Thread, msg string) {
			log.Infof("thread:%s %s", thread.Name, msg)
		},
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, fmt.Errorf("load is not allowed in handler")
		},
	}

	hctx := starlarkstruct.FromStringDict(starlark.String("ctx"), map[string]starlark.Value{
		"actions":  starCmdActions(ctx, cmd),
		"metadata": starMetadata(cfg.Metadata),
		"flags":    starFlags(cfg.flags),
		"fs":       starFS(ctx, cmd.HashFS.FileSystem(ctx, cmd.ExecRoot), bpath, cfg.fscache),
	})

	hcmd, err := packCmd(cmd, expandedInputs)
	if err != nil {
		return fmt.Errorf("failed to pack cmd: %w", err)
	}
	// hctx and hcmd will be frozen, so fun may not mutate hcmd.
	_, err = starlark.Call(thread, fun, []starlark.Value{hctx, hcmd}, nil)
	if err != nil {
		log.Warnf("thread:%s failed to run %s: %v", thread.Name, handler, err)
		var eerr *starlark.EvalError
		if errors.As(err, &eerr) {
			log.Warnf("stacktrace:\n%s", eerr.Backtrace())
			return HandlerError{entry: handler, fn: fun, err: eerr}

		}
		return fmt.Errorf("failed to run %s: %w", handler, err)
	}
	return nil
}
