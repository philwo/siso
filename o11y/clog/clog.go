// Copyright 2023 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package clog provides context aware logging.
// It can store trace, spandID, arbitrary labels to each context.
// The main use case is to add build action context to each log entry automatically.
//
// TODO(b/267575656): It uses Cloud logging.Entry since it's intended to support Cloud Logging integration.
// However, it currently supports local logging with glog.
// It's also worth considering to use go.chromium.org/luci/common/logging and add the Cloud Logging integration there.
package clog

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/logging"
	"github.com/golang/glog"
)

type contextKeyType int

var contextKey contextKeyType

// defaultFormatter doesn't set any context to the log content.
var defaultFormatter = func(e logging.Entry) string {
	return fmt.Sprintf("%v", e.Payload)
}

// New creates a new Logger.
func New(ctx context.Context) *Logger {
	return &Logger{
		Formatter: defaultFormatter,
	}
}

// NewContext sets the given logger to the context.
func NewContext(ctx context.Context, logger *Logger) context.Context {
	return context.WithValue(ctx, contextKey, logger)
}

// NewSpan sets a new logger.Span with the given labels to the context.
func NewSpan(ctx context.Context, trace, spanID string, labels map[string]string) context.Context {
	logger, _ := ctx.Value(contextKey).(*Logger)
	return NewContext(ctx, logger.Span(trace, spanID, labels))
}

// FromContext returns a logger in the context, or nil if it's not set.
func FromContext(ctx context.Context) *Logger {
	logger, ok := ctx.Value(contextKey).(*Logger)
	if !ok {
		return nil
	}
	return logger
}

// Logger holds the trace, spanID, arbitrary labels of the context.
// It also can have custom formatter to generate a log content.
type Logger struct {
	// Formatter is a formatter of the entry for glog.
	// Default to `fmt.Sprintf("%v", e.Payload)`.
	Formatter func(e logging.Entry) string

	// The following properties are equivalent to the ones in logging.LogEntry.
	// See the document for the details.
	// https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry
	trace  string
	spanID string
	labels map[string]string
}

// Span returns a sub logger for the trace span.
func (l *Logger) Span(trace, spanID string, labels map[string]string) *Logger {
	return &Logger{
		Formatter: l.Formatter,
		trace:     trace,
		spanID:    spanID,
		labels:    labels,
	}
}

func (l *Logger) log(e logging.Entry) {
	l.glogEntry(e)
}

func (l *Logger) glogEntry(e logging.Entry) {
	msg := l.Formatter(e)
	switch e.Severity {
	case logging.Info:
		glog.InfoDepth(3, msg)
	case logging.Warning:
		glog.WarningDepth(3, msg)
	case logging.Error:
		glog.ErrorDepth(3, msg)
	case logging.Critical:
		glog.FatalDepth(3, msg)
	case logging.Emergency:
		glog.ExitDepth(3, msg)
	default:
		glog.InfoDepth(3, fmt.Sprintf("%s %s", e.Severity, msg))
	}
}

// Info logs at info log level in the manner of fmt.Print.
func (l *Logger) Info(args ...interface{}) {
	l.log(l.Entry(logging.Info, fmt.Sprint(args...)))
}

// Infoln logs at info log level in the manner of fmt.Println.
func (l *Logger) Infoln(args ...interface{}) {
	l.log(l.Entry(logging.Info, fmt.Sprintln(args...)))
}

// Infof logs at info log level in the manner of fmt.Printf.
func (l *Logger) Infof(format string, args ...interface{}) {
	l.log(l.Entry(logging.Info, fmt.Sprintf(format, args...)))
}

// Infof logs at info log level in the manner of fmt.Printf.
func Infof(ctx context.Context, format string, args ...interface{}) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Info, fmt.Sprintf(format, args...)))
}

// Warning logs at warning log level in the manner of fmt.Print.
func (l *Logger) Warning(args ...interface{}) {
	l.log(l.Entry(logging.Warning, fmt.Sprint(args...)))
}

// Warningln logs at warning log level in the manner of fmt.Println.
func (l *Logger) Warningln(args ...interface{}) {
	l.log(l.Entry(logging.Warning, fmt.Sprintln(args...)))
}

// Warningf logs at warning log level in the manner of fmt.Printf.
func (l *Logger) Warningf(format string, args ...interface{}) {
	l.log(l.Entry(logging.Warning, fmt.Sprintf(format, args...)))
}

// Warningf logs at warning log level in the manner of fmt.Printf.
func Warningf(ctx context.Context, format string, args ...interface{}) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Warning, fmt.Sprintf(format, args...)))
}

// Error logs at error log level in the manner of fmt.Print.
func (l *Logger) Error(args ...interface{}) {
	l.log(l.Entry(logging.Error, fmt.Sprint(args...)))
}

// Errorln logs at error log level in the manner of fmt.Println.
func (l *Logger) Errorln(args ...interface{}) {
	l.log(l.Entry(logging.Error, fmt.Sprintln(args...)))
}

// Errorf logs at error log level in the manner of fmt.Printf.
func (l *Logger) Errorf(format string, args ...interface{}) {
	l.log(l.Entry(logging.Error, fmt.Sprintf(format, args...)))
}

// Errorf logs at error log level in the manner of fmt.Printf.
func Errorf(ctx context.Context, format string, args ...interface{}) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Warning, fmt.Sprintf(format, args...)))
}

// Fatal logs at fatal log level in the manner of fmt.Print with stacktrace, and exit.
func (l *Logger) Fatal(args ...interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.fatalf(ctx, "%s", fmt.Sprint(args...))
}

// Fatalln logs at fatal log level in the manner of fmt.Println with stacktrace, and exit.
func (l *Logger) Fatalln(args ...interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.fatalf(ctx, "%s", fmt.Sprintln(args...))
}

// Fatalf logs at fatal log level in the manner of fmt.Printf with stacktrace, and exit.
func (l *Logger) Fatalf(format string, args ...interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.fatalf(ctx, format, args...)
}

func (l *Logger) fatalf(ctx context.Context, format string, args ...interface{}) {
	l.log(l.Entry(logging.Critical, fmt.Sprintf(format, args...)))
}

// Fatalf logs at fatal log level in the manner of fmt.Printf with stacktrace, and exit.
func Fatalf(ctx context.Context, format string, args ...interface{}) {
	logger := FromContext(ctx)
	logger.log(logger.Entry(logging.Critical, fmt.Sprintf(format, args...)))
}

// Exitf logs at fatal log level in the manner of fmt.Printf, and exit.
func (l *Logger) Exitf(format string, args ...interface{}) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l.exitf(ctx, format, args...)
}

func (l *Logger) exitf(ctx context.Context, format string, args ...interface{}) {
	l.log(l.Entry(logging.Emergency, fmt.Sprintf(format, args...)))
}

// Exitf logs at fatal log level in the manner of fmt.Printf, and exit.
func Exitf(ctx context.Context, format string, args ...interface{}) {
	logger := FromContext(ctx)
	logger.exitf(ctx, format, args...)
}

// Entry creates a new log entry for the given severity.
func (l *Logger) Entry(severity logging.Severity, payload interface{}) logging.Entry {
	return logging.Entry{
		Timestamp: time.Now(),
		Severity:  severity,
		Payload:   payload,
		Labels:    l.labels,
		// TODO(b/267575656): Add source location.
		SourceLocation: nil,
		Trace:          l.trace,
		SpanID:         l.spanID,
	}
}

// V checks at verbose log level.
func (l *Logger) V(level int) bool {
	return bool(glog.V(glog.Level(level)))
}

// Close closes the logger. it will flush log entries.
func (l *Logger) Close() {
	glog.Flush()
}
