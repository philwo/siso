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
	"runtime"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"infra/build/siso/build"
	"infra/build/siso/hashfs"
	"infra/build/siso/o11y/clog"
)

const configEntryPoint = "init"

// Config is a build config.
type Config struct {
	// ID is an id of the build process (toolchain invocation ID).
	ID string

	// Metadata is a metadata of the build.
	Metadata build.Metadata

	// flags used to run the build.
	flags map[string]string

	// global variables loaded by the config.
	globals map[string]starlark.Value

	// handlers registered by the config.
	handlers *starlark.Dict

	// TODO(b/266518906): add filegroups

	// filesystem cache used for handlers.
	fscache *fscache
}

// New returns new build config.
func New(ctx context.Context, fname string, flags map[string]string, repos map[string]fs.FS) (*Config, error) {
	metadata := build.Metadata{
		KV:     make(map[string]string),
		NumCPU: runtime.NumCPU(),
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
	}
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
		predeclared: builtinModule(ctx),
	}
	clog.Infof(ctx, "enable starlark recursion")
	resolve.AllowRecursion = true

	thread := &starlark.Thread{
		Name: "load",
		Print: func(thread *starlark.Thread, msg string) {
			clog.Infof(ctx, "thread:%s %s", thread.Name, msg)
		},
		Load: loader.Load,
	}
	thread.SetLocal("modulename", fname)
	sdict, err := loader.Load(thread, fname)
	if err != nil {
		clog.Warningf(ctx, "thread:%s failed to exec file %s: %v", thread.Name, fname, err)
		var eerr *starlark.EvalError
		if errors.As(err, &eerr) {
			clog.Warningf(ctx, "stacktrace:\n%s", eerr.Backtrace())
		}
		return nil, err
	}
	clog.Infof(ctx, "config: %s", sdict)
	v, ok := sdict[configEntryPoint]
	if !ok {
		return nil, fmt.Errorf("%s is not defined in %s", configEntryPoint, fname)
	}
	if _, ok := v.(starlark.Callable); !ok {
		return nil, fmt.Errorf("%s %T is not callable in %s", configEntryPoint, v.Type(), fname)
	}
	return &Config{
		Metadata: metadata,
		flags:    flags,
		globals:  sdict,
		fscache: &fscache{
			m: make(map[string][]byte),
		},
	}, nil
}

// Init initializes config by running `init`.
func (cfg *Config) Init(ctx context.Context, hashFS *hashfs.HashFS, buildPath *build.Path) (string, error) {
	fun, ok := cfg.globals[configEntryPoint]
	if !ok {
		return "", fmt.Errorf("no %s", configEntryPoint)
	}
	thread := &starlark.Thread{
		Name: configEntryPoint,
		Print: func(thread *starlark.Thread, msg string) {
			clog.Infof(ctx, "thread:%s %s", thread.Name, msg)
		},
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, fmt.Errorf("load is not allowed in init")
		},
	}

	hctx := starlarkstruct.FromStringDict(starlark.String("ctx"), map[string]starlark.Value{
		"actions":  starInitActions(cfg.Metadata.KV),
		"metadata": starMetadata(cfg.Metadata.KV),
		"flags":    starFlags(cfg.flags),
		// want "envs" ?
		"fs": starFS(ctx, hashFS.FileSystem(ctx, buildPath.ExecRoot), buildPath, cfg.fscache),
	})
	clog.Infof(ctx, "hctx: %v", hctx)
	ret, err := starlark.Call(thread, fun, starlark.Tuple([]starlark.Value{hctx}), nil)
	if err != nil {
		clog.Warningf(ctx, "thread:%s failed to run %s: %v", thread.Name, configEntryPoint, err)
		var eerr *starlark.EvalError
		if errors.As(err, &eerr) {
			clog.Warningf(ctx, "stacktrace:\n%s", eerr.Backtrace())
		}
		return "", fmt.Errorf("failed to run %s: %w", configEntryPoint, err)
	}
	m, ok := ret.(*starlarkstruct.Module)
	if !ok {
		return "", fmt.Errorf("%s returned %s, want module", configEntryPoint, ret.Type())
	}
	h, err := m.Attr("handlers")
	if err != nil {
		return "", fmt.Errorf("no handlers in %v", ret)
	}
	handlers, ok := h.(*starlark.Dict)
	if !ok {
		return "", fmt.Errorf("handlers %v, want dict", h)
	}
	cfg.handlers = handlers

	// TODO(b/266518906): add filegroups

	stepConfig, err := m.Attr("step_config")
	if err != nil {
		return "", fmt.Errorf("no step_config in %v", ret)
	}
	s, ok := starlark.AsString(stepConfig)
	if !ok {
		return "", fmt.Errorf("%s returned %s, want string", configEntryPoint, ret.Type())
	}
	return s, nil
}
