// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package msvcutil

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
)

// ScanDepsParams parses args and returns files, dirs, sysroots and defines
// for scandeps.
// It only parses major command line flags used in chromium.
// full set of command line flags for include dirs can be found in
// https://learn.microsoft.com/en-us/cpp/build/reference/compiler-options-listed-by-category?view=msvc-170
// https://clang.llvm.org/docs/ClangCommandLineReference.html#include-path-management
func ScanDepsParams(ctx context.Context, args, env []string) (files, dirs, sysroots []string, defines map[string]string, err error) {
	defines = make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if runtime.GOOS != "windows" {
				arg = strings.ReplaceAll(arg, `\`, "/")
			}
			cmdname := filepath.Base(arg)
			cmdname = strings.TrimSuffix(cmdname, filepath.Ext(cmdname))
			if cmdname == "clang-cl" {
				// add toolchain top dir as sysroots too
				// cl.exe has no such semantics?
				sysroots = append(sysroots, filepath.ToSlash(filepath.Dir(filepath.Dir(arg))))
			}
		}
		switch arg {
		case "-I", "/I":
			i++
			dirs = append(dirs, filepath.ToSlash(args[i]))
			continue
		case "-D", "/D":
			i++
			defineMacro(defines, args[i])
			continue
		}
		switch {
		case strings.HasPrefix(arg, "-I"):
			dirs = append(dirs, filepath.ToSlash(strings.TrimPrefix(arg, "-I")))
		case strings.HasPrefix(arg, "/I"):
			dirs = append(dirs, filepath.ToSlash(strings.TrimPrefix(arg, "/I")))

		case strings.HasPrefix(arg, "-D"):
			defineMacro(defines, strings.TrimPrefix(arg, "-D"))
		case strings.HasPrefix(arg, "/D"):
			defineMacro(defines, strings.TrimPrefix(arg, "/D"))

		case strings.HasPrefix(arg, "/winsysroot"):
			sysroots = append(sysroots, filepath.ToSlash(strings.TrimPrefix(arg, "/winsysroot")))
		case !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/"):
			ext := filepath.Ext(arg)
			switch ext {
			case ".c", ".cc", ".cxx", ".cpp", ".S":
				files = append(files, filepath.ToSlash(arg))
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
