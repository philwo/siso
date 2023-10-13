// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strings"
	"time"

	"infra/build/siso/o11y/clog"
)

// File name of ninja log.
const ninjaLogName = ".ninja_log"

// Ninja log format version.
const ninjaLogVersion = 5

// OpenNinjaLog opens ninja log file or creates a new file with a version header.
func OpenNinjaLog(ctx context.Context) (*os.File, error) {
	f, err := os.OpenFile(ninjaLogName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		e := f.Close()
		if e != nil {
			clog.Errorf(ctx, "%v", e)
		}
		return nil, err
	}
	if fi.Size() == 0 {
		fmt.Fprintf(f, "# ninja log v%d\n", ninjaLogVersion)
	}
	return f, nil
}

// WriteNinjaLogEntries writes ninja log entries for a command.
// Note that the log entries are not compatible with Ninja, yet.
// TODO: b/298594790
//   - Implement MurmurHash64A as Ninja.
//   - Make mtime compatible on Windows.
func WriteNinjaLogEntries(ctx context.Context, w io.Writer, start, end int64, mtime time.Time, outputs, command []string) {
	h := fnv.New64()
	h.Write([]byte(strings.Join(command, " ")))
	hash := hex.EncodeToString(h.Sum(nil))
	for _, out := range outputs {
		fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%s\n", start, end, mtime.UnixNano(), out, hash)
	}
}
