// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

// CPPScan scans C preprocessor directives for #include/#define in buf.
func CPPScan(fname string, buf []byte) ([]string, map[string][]string, error) {
	started := time.Now()

	var includes []string
	defines := make(map[string][]string)
	for len(buf) > 0 {
		// start of line
		buf = bytes.TrimSpace(buf)
		if len(buf) == 0 {
			break
		}
		var line []byte
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			line = buf
			buf = nil
		} else {
			line = buf[:i]
			buf = buf[i+1:]
		}
		lineStart := line
		if line[0] != '#' {
			// not directive line
			logLine := line
			log.Debugf("skip %q", logLine)
			continue
		}
		// skip #
		line = line[1:]
		line = bytes.TrimSpace(line)

		switch {
		case bytes.HasPrefix(line, []byte("include")):
			line = bytes.TrimPrefix(line, []byte("include"))
			switch {
			case bytes.HasPrefix(line, []byte("_next")):
				// #include_next
				line = bytes.TrimPrefix(line, []byte("_next"))
			case line[0] == ' ':
			case line[0] == '\t':
			default:
				// not '#include ' nor '#include_next ' ?
				logLineStart := lineStart
				log.Debugf("skip %q", logLineStart)
				continue
			}
		case bytes.HasPrefix(line, []byte("import")):
			line = bytes.TrimPrefix(line, []byte("import"))
			switch line[0] {
			case ' ', '\t':
			default:
				logLineStart := lineStart
				log.Debugf("skip %q", logLineStart)
				continue
			}

		case bytes.HasPrefix(line, []byte("define")):
			line = bytes.TrimPrefix(line, []byte("define"))
			switch line[0] {
			case ' ', '\t':
			default:
				// not '#define '
				logLineStart := lineStart
				log.Debugf("skip %q", logLineStart)
				continue
			}
			line = bytes.TrimSpace(line)
			addDefine(defines, fname, line)
			continue
		default:
			// ignore other directives
			logLineStart := lineStart
			log.Debugf("skip %q", logLineStart)
			continue
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			// no path for #include?
			logLineStart := lineStart
			log.Debugf("skip %q", logLineStart)
			continue
		}
		includes = addInclude(includes, line)
	}
	dur := time.Since(started)
	if dur > time.Second {
		log.Infof("slow cppScan %s %s", fname, dur)
	}
	return includes, defines, nil
}

func cppExpandMacros(ctx context.Context, paths []string, incname string, macros map[string][]string) []string {
	if incname == "" {
		return nil
	}
	if !isMacro(incname) {
		return append(paths, incname)
	}
	values, ok := macros[incname]
	if !ok {
		return nil
	}
	for _, v := range values {
		paths = cppExpandMacros(ctx, paths, v, macros)
	}
	log.Debugf("expand %q -> %q", incname, paths)
	return paths
}

func addInclude(paths []string, incpath []byte) []string {
	logIncpath := incpath
	log.Debugf("addInclude %q", logIncpath)
	delim := string(incpath[0])
	switch delim {
	case `"`:
	case `<`:
		delim = ">"
	default:
		delim = " \t"
	}
	i := bytes.IndexAny(incpath[1:], delim)
	if i < 0 {
		if delim == ">" || delim == `"` {
			// unclosed path?
			logIncpath := incpath
			log.Debugf("unclosed path? %q", logIncpath)
			return paths
		}
		// otherwise, use rest of line as token.
	} else if delim == `"` || delim == ">" {
		incpath = incpath[:i+2] // include delim both side.
	} else {
		incpath = incpath[:i+1]
	}
	if incpath[0] != '"' && incpath[0] != '<' && (incpath[0] < 'A' || incpath[0] > 'Z') {
		// not <>, "", nor upper macros?
		return paths
	}
	logIncpath = incpath
	log.Debugf("include %q", logIncpath)
	return append(paths, strings.Clone(string(incpath)))
}

func addDefine(defines map[string][]string, fname string, line []byte) {
	// line
	//  MACRO "path.h"
	//  MACRO <path.h>
	i := bytes.IndexAny(line, " \t")
	if i < 0 {
		// no macro name
		logLine := line
		log.Debugf("no macro name: %q", logLine)
		return
	}
	macro := strings.Clone(string(line[:i]))
	if strings.Contains(macro, "(") {
		logMacro := macro
		log.Debugf("ignore func maro: %q", logMacro)
		return
	}
	line = bytes.TrimSpace(line[i+1:])
	if len(line) == 0 {
		logMacro := macro
		log.Debugf("no macro value for %q?", logMacro)
		return
	}
	switch line[0] {
	case '<', '"':
		delim := line[0]
		if delim == '<' {
			delim = '>'
		}
		i = bytes.IndexByte(line[1:], delim)
		if i < 0 {
			// unclosed path?
			lv := struct {
				macro string
				line  []byte
			}{macro: macro, line: line}
			log.Debugf("unclosed path for macro %q: %q", lv.macro, lv.line)
			return
		}
		value := strings.Clone(string(line[:i+2])) // include delim at both side
		defines[macro] = append(defines[macro], value)
	default:
		// not "path.h" or <path.h>
		// support only one token
		// e.g.
		//  #define FT_DRIVER_H <freetype/ftdriver.h>
		//  #define FT_AUTHHINTER_H FT_DRIVER_H
		//
		// support only capital letter token.
		value := line
		i = bytes.IndexAny(value, " \t")
		if i >= 0 {
			value = value[:i]
		}
		if len(value) == 0 {
			logMacro := macro
			log.Debugf("just define macro %s?", logMacro)
			return
		}
		if bytes.IndexByte(value, '(') >= 0 {
			lv := struct {
				macro string
				value []byte
			}{macro: macro, value: value}
			log.Debugf("ignore func maro: %q=%q", lv.macro, lv.value)
			return
		}
		if value[0] >= 'A' && value[0] <= 'Z' {
			defines[macro] = append(defines[macro], strings.Clone(string(value)))
		} else {
			lv := struct {
				macro string
				line  []byte
			}{macro: macro, line: line}
			log.Debugf("ignore macro %s=%s", lv.macro, lv.line)
		}
	}
}

func isMacro(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case '<', '"':
		return false
	}
	return true
}
