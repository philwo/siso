// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package trace manages execution traces.
package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sync"
	"time"

	"cloud.google.com/go/trace/apiv2/tracepb"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"infra/build/siso/o11y/clog"
)

// Context is a trace context.
type Context struct {
	// https://cloud.google.com/trace/docs/reference/v2/rest/v2/projects.traces/batchWrite#Span
	// [TRACE_ID] is a unique identifier for a trace within a project;
	// it is a 32-character hexadecimal encoding of a 16-byte array.
	// It should not be zero.
	traceID [16]byte

	mu sync.Mutex
	// first span is the top span in the trace.
	spans []*Span
}

// New creates a new context for id (uuid).
func New(ctx context.Context, id string) *Context {
	if log.V(2) {
		clog.Infof(ctx, "new trace context for %s", id)
	}
	u, err := uuid.Parse(id)
	if err != nil {
		clog.Errorf(ctx, "bad id %q: %v", id, err)
	}
	return &Context{
		traceID: ([16]byte)(u),
	}
}

// NewSpan creates new span in the parent.
func (t *Context) NewSpan(ctx context.Context, name string, parent *Span) *Span {
	if t == nil {
		return nil
	}
	return t.newSpan(ctx, name, parent)
}

// Spans returns span data in the trace context.
func (t *Context) Spans() []SpanData {
	var data []SpanData
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.spans {
		sd := s.data()
		if sd.Name == "" {
			continue
		}
		data = append(data, sd)
	}
	return data
}

// SpanProtos returns span protos in the trace context.
func (t *Context) SpanProtos(ctx context.Context, projectID string) []*tracepb.Span {
	var spans []*tracepb.Span
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.spans {
		span := s.proto(ctx, projectID)
		if span == nil {
			continue
		}
		spans = append(spans, span)
	}
	return spans
}

func (t *Context) newSpan(ctx context.Context, name string, parent *Span) *Span {
	var spanID [8]byte
	t.mu.Lock()
	defer t.mu.Unlock()
	id := fmt.Sprintf("%s-%d", name, len(t.spans))
	if parent == nil && len(t.spans) > 0 {
		parent = t.spans[0]
	}
	s := sha256.Sum256([]byte(id))
	copy(spanID[:], s[:])
	span := &Span{
		t:           t,
		spanID:      spanID,
		parent:      parent,
		displayName: name,
		start:       time.Now(),
		attrs:       make(map[string]any),
	}
	if log.V(2) {
		clog.Infof(ctx, "new span %s %q<%v", name, spanID, parent)
	}
	t.spans = append(t.spans, span)
	return span
}

type contextKeyType int

const (
	contextKey contextKeyType = iota
	spanKey
)

// NewContext returns new context with a trace context.
func NewContext(ctx context.Context, t *Context) context.Context {
	return context.WithValue(ctx, contextKey, t)
}

// NewSpan returns new contexts and span.
// If no trace context, returns nil span.
func NewSpan(ctx context.Context, name string) (context.Context, *Span) {
	t, ok := ctx.Value(contextKey).(*Context)
	if !ok || t == nil {
		return ctx, nil
	}
	parent, _ := ctx.Value(spanKey).(*Span)
	span := t.NewSpan(ctx, name, parent)
	return context.WithValue(ctx, spanKey, span), span
}

// ID returns the trace id.
func ID(ctx context.Context) string {
	t, ok := ctx.Value(contextKey).(*Context)
	if !ok {
		return ""
	}
	return uuid.UUID(t.traceID).String()
}

// CurSpan returns current span in the context.
func CurSpan(ctx context.Context) *Span {
	span, ok := ctx.Value(spanKey).(*Span)
	if !ok {
		return nil
	}
	return span
}

// Span is a trace span.
type Span struct {
	t      *Context
	spanID [8]byte
	parent *Span

	mu          sync.Mutex
	displayName string
	start       time.Time
	end         time.Time
	attrs       map[string]any
	status      *spb.Status
}

// SetAttr sets attributes in the span.
func (s *Span) SetAttr(key string, value any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs[key] = value
}

// Add adds span data as a child of the span and returns it.
func (s *Span) Add(ctx context.Context, sd SpanData) *Span {
	if s == nil {
		return nil
	}
	if s.t == nil {
		return nil
	}
	s.mu.Lock()
	ss := s.t.newSpan(ctx, sd.Name, s)
	s.mu.Unlock()
	ss.start = sd.Start
	ss.end = sd.End
	ss.attrs = sd.Attrs
	ss.status = sd.Status
	return ss
}

// Close closes the span.
func (s *Span) Close(st *spb.Status) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end = time.Now()
	s.status = st
}

func (s *Span) protoAttrs() *tracepb.Span_Attributes {
	if s == nil {
		return nil
	}
	m := make(map[string]*tracepb.AttributeValue)
	for k, v := range s.attrs {
		av := attrValue(v)
		if av != nil {
			m[k] = av
		}
	}
	return &tracepb.Span_Attributes{
		AttributeMap: m,
	}
}

func (s *Span) data() SpanData {
	if s == nil {
		return SpanData{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.end
	if end.IsZero() {
		end = time.Now()
	}
	return SpanData{
		Name:   s.displayName,
		Start:  s.start,
		End:    end,
		Attrs:  s.attrs,
		Status: s.status,
	}
}

// ID returns trace and span id.
func (s *Span) ID(projectID string) (trace, span string) {
	return path.Join("projects", projectID, "traces", hex.EncodeToString(s.t.traceID[:])), hex.EncodeToString(s.spanID[:])
}

func (s *Span) proto(ctx context.Context, projectID string) *tracepb.Span {
	if s == nil {
		return nil
	}
	if projectID == "" {
		return nil
	}
	spanID := hex.EncodeToString(s.spanID[:])
	parent := ""
	if s.parent != nil {
		parent = hex.EncodeToString(s.parent.spanID[:])
		if spanID == parent {
			clog.Fatalf(ctx, "spanID == parent? %q span=%p[%q] parent=%p[%q]", spanID, s, s.displayName, s.parent, s.parent.displayName)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &tracepb.Span{
		Name: path.Join("projects", projectID,
			"traces", hex.EncodeToString(s.t.traceID[:]),
			"spans", spanID),
		SpanId:       spanID,
		ParentSpanId: parent,
		DisplayName:  truncatableString(s.displayName, 128),
		StartTime:    timestamppb.New(s.start),
		EndTime:      timestamppb.New(s.end),
		Attributes:   s.protoAttrs(),
		Status:       s.status,
		SpanKind:     tracepb.Span_INTERNAL,
	}
}

func truncatableString(s string, limit int) *tracepb.TruncatableString {
	n := len(s)
	if n < limit {
		return &tracepb.TruncatableString{Value: s}
	}
	return &tracepb.TruncatableString{
		Value:              s[:limit],
		TruncatedByteCount: int32(n - limit),
	}
}

func attrValue(v any) *tracepb.AttributeValue {
	var iv int64
	switch v := v.(type) {
	case []string:
		return nil
	case string:
		return &tracepb.AttributeValue{
			Value: &tracepb.AttributeValue_StringValue{StringValue: truncatableString(v, 256)},
		}
	case bool:
		return &tracepb.AttributeValue{
			Value: &tracepb.AttributeValue_BoolValue{BoolValue: v},
		}
	case int:
		iv = int64(v)
	case uint:
		iv = int64(v)
	case int8:
		iv = int64(v)
	case uint8:
		iv = int64(v)
	case int16:
		iv = int64(v)
	case uint16:
		iv = int64(v)
	case int32:
		iv = int64(v)
	case uint32:
		iv = int64(v)
	case int64:
		iv = v
	case uint64:
		iv = int64(v)
	}
	return &tracepb.AttributeValue{
		Value: &tracepb.AttributeValue_IntValue{IntValue: iv},
	}
}

// SpanData is a span data.
type SpanData struct {
	Name   string
	Start  time.Time
	End    time.Time
	Attrs  map[string]any
	Status *spb.Status
}

// Duration returns duration of the span.
func (sd SpanData) Duration() time.Duration {
	return sd.End.Sub(sd.Start)
}
