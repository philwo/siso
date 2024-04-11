// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package gccutil

import (
	"context"
	"path/filepath"
	"strings"
)

// ScanDepsParams holds parameters used for scandeps.
type ScanDepsParams struct {
	// Sources are source files.
	Sources []string

	// Includes are include files by -include.
	Includes []string

	// Files are input files, such as sanitaizer ignore list.
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

// ExtractScanDepsParams parses args and returns ScanDepsParams for scandeps.
// It only parses major command line flags used in chromium.
// full set of command line flags for include dirs can be found in
// https://clang.llvm.org/docs/ClangCommandLineReference.html#include-path-management
func ExtractScanDepsParams(ctx context.Context, args, env []string) ScanDepsParams {
	res := ScanDepsParams{
		Defines: make(map[string]string),
	}
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
				res.Sysroots = append(res.Sysroots, filepath.ToSlash(filepath.Dir(filepath.Dir(arg))))
			}
		}
		switch arg {
		case "-I", "--include-directory", "-isystem", "-iquote":
			i++
			res.Dirs = append(res.Dirs, args[i])
			continue
		case "-F", "-iframework":
			i++
			res.Frameworks = append(res.Frameworks, args[i])
			continue
		case "-include":
			i++
			res.Includes = append(res.Includes, args[i])
			continue
		case "-isysroot":
			i++
			res.Sysroots = append(res.Sysroots, args[i])
			continue
		case "-D":
			i++
			defineMacro(res.Defines, args[i])
			continue
		}
		switch {
		case strings.HasPrefix(arg, "-I"):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "-I"))
		case strings.HasPrefix(arg, "--include-directory="):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "--include-directory="))
		case strings.HasPrefix(arg, "-iquote"):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "-iquote"))
		case strings.HasPrefix(arg, "-isystem"):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "-isystem"))
		case strings.HasPrefix(arg, "-F"):
			res.Frameworks = append(res.Frameworks, strings.TrimPrefix(arg, "-F"))
		case strings.HasPrefix(arg, "-fprofile-use="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fprofile-use="))
		case strings.HasPrefix(arg, "-fsanitize-ignorelist="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fsanitize-ignorelist="))
		case strings.HasPrefix(arg, "-iframework"):
			res.Frameworks = append(res.Frameworks, strings.TrimPrefix(arg, "-iframework"))
		case strings.HasPrefix(arg, "--sysroot="):
			res.Sysroots = append(res.Sysroots, strings.TrimPrefix(arg, "--sysroot="))
		case strings.HasPrefix(arg, "-D"):
			defineMacro(res.Defines, strings.TrimPrefix(arg, "-D"))

		case !strings.HasPrefix(arg, "-"):
			ext := filepath.Ext(arg)
			switch ext {
			case ".c", ".cc", ".cxx", ".cpp", ".m", ".mm", ".S":
				res.Sources = append(res.Sources, arg)
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
