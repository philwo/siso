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

// ScanDepsParams holds parameters used for scandeps.
type ScanDepsParams struct {
	// Sources are source files.
	Sources []string

	// Includes are include file specified by -include or /FI.
	Includes []string

	// Files are input files, such as sanitizer ignore list.
	Files []string

	// Dirs are include directories.
	Dirs []string

	// Frameworks are framework directories.
	Frameworks []string

	// Sysroots are sysroot directories and toolchain root directories.
	Sysroots []string

	// Defines are defined macros.
	Defines map[string]string
}

// ExtractScanDepsParams parses args and returns files, dirs, sysroots and defines
// for scandeps.
// It only parses major command line flags used in chromium.
// full set of command line flags for include dirs can be found in
// https://learn.microsoft.com/en-us/cpp/build/reference/compiler-options-listed-by-category?view=msvc-170
// https://clang.llvm.org/docs/ClangCommandLineReference.html#include-path-management
func ExtractScanDepsParams(ctx context.Context, args, env []string) ScanDepsParams {
	res := ScanDepsParams{
		Defines: make(map[string]string),
	}
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
				res.Sysroots = append(res.Sysroots, filepath.ToSlash(filepath.Dir(filepath.Dir(arg))))
			}
		}
		switch arg {
		case "-I", "/I":
			i++
			res.Dirs = append(res.Dirs, filepath.ToSlash(args[i]))
			continue
		case "-D", "/D":
			i++
			defineMacro(res.Defines, args[i])
			continue
		case "-FI", "/FI":
			i++
			res.Includes = append(res.Includes, filepath.ToSlash(args[i]))
			continue
		}
		switch {
		case strings.HasPrefix(arg, "-I"):
			res.Dirs = append(res.Dirs, filepath.ToSlash(strings.TrimPrefix(arg, "-I")))
		case strings.HasPrefix(arg, "/I"):
			res.Dirs = append(res.Dirs, filepath.ToSlash(strings.TrimPrefix(arg, "/I")))

		case strings.HasPrefix(arg, "-D"):
			defineMacro(res.Defines, strings.TrimPrefix(arg, "-D"))
		case strings.HasPrefix(arg, "/D"):
			defineMacro(res.Defines, strings.TrimPrefix(arg, "/D"))

		case strings.HasPrefix(arg, "-FI"):
			res.Includes = append(res.Includes, filepath.ToSlash(strings.TrimPrefix(arg, "-FI")))
		case strings.HasPrefix(arg, "/FI"):
			res.Includes = append(res.Includes, filepath.ToSlash(strings.TrimPrefix(arg, "/FI")))

		case strings.HasPrefix(arg, "-fprofile-use="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fprofile-use="))
		case strings.HasPrefix(arg, "-fsanitize-ignorelist="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fsanitize-ignorelist="))

		case strings.HasPrefix(arg, "/winsysroot"):
			res.Sysroots = append(res.Sysroots, filepath.ToSlash(strings.TrimPrefix(arg, "/winsysroot")))
		case !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/"):
			ext := filepath.Ext(arg)
			switch ext {
			case ".c", ".cc", ".cxx", ".cpp", ".S":
				res.Sources = append(res.Sources, filepath.ToSlash(arg))
			}
		}
	}
	return res
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
