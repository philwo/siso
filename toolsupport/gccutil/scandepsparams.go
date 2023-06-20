// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package gccutil

import (
	"context"
	"path/filepath"
	"strings"
)

// ScanDepsParams parses args and returns files, dirs, sysroots and defines
// for scandeps.
// It only parses major command line flags used in chromium.
// full set of command line flags for include dirs can be found in
// https://clang.llvm.org/docs/ClangCommandLineReference.html#include-path-management
func ScanDepsParams(ctx context.Context, args, env []string) (files, dirs, sysroots []string, defines map[string]string, err error) {
	defines = make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			cmdname := filepath.Base(arg)
			switch {
			case strings.HasSuffix(cmdname, "clang"),
				strings.HasSuffix(cmdname, "clang++"),
				strings.HasSuffix(cmdname, "clang-cl"),
				strings.HasSuffix(cmdname, "clang-cl.exe"),
				strings.HasSuffix(cmdname, "gcc"),
				strings.HasSuffix(cmdname, "g++"):
				// add toolchain top dir as sysroots too
				sysroots = append(sysroots, filepath.ToSlash(filepath.Dir(filepath.Dir(arg))))
			}
		}
		switch arg {
		case "-I", "--include-directory", "-isystem", "-iquote":
			i++
			dirs = append(dirs, args[i])
			continue
		case "-D":
			i++
			defineMacro(defines, args[i])
			continue
		}
		switch {
		case strings.HasPrefix(arg, "-I"):
			dirs = append(dirs, strings.TrimPrefix(arg, "-I"))
		case strings.HasPrefix(arg, "--include-directory="):
			dirs = append(dirs, strings.TrimPrefix(arg, "--include-directory="))
		case strings.HasPrefix(arg, "-iquote"):
			dirs = append(dirs, strings.TrimPrefix(arg, "-iquote"))
		case strings.HasPrefix(arg, "-isystem"):
			dirs = append(dirs, strings.TrimPrefix(arg, "-isystem"))
		case strings.HasPrefix(arg, "--sysroot="):
			sysroots = append(sysroots, strings.TrimPrefix(arg, "--sysroot="))
		case strings.HasPrefix(arg, "-D"):
			defineMacro(defines, strings.TrimPrefix(arg, "-D"))

		case !strings.HasPrefix(arg, "-"):
			ext := filepath.Ext(arg)
			switch ext {
			case ".c", ".cc", ".cxx", ".cpp", ".m", ".mm", ".S":
				files = append(files, arg)
			}
		}
	}
	return files, dirs, sysroots, defines, nil
}

func defineMacro(defines map[string]string, arg string) {
	// arg: macro=value
	macro, value, ok := strings.Cut(arg, "=")
	if !ok {
		// just `-D MACRO`
		return
	}
	if value == "" {
		// `-D MACRO=`
		// no value
		return
	}
	switch value[0] {
	case '<', '"':
		// <path.h> or "path.h"?
		defines[macro] = value
	}
}
