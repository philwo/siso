// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package monitoring provides Cloud Monitoring (aka Stackdriver) support.
// It reuses the same stats with Reclient uses so that the existing dashboards
// and alerts can be shared.
// See also https://github.com/bazelbuild/reclient/blob/4d9d00de3f05c24ce2af03455243bed45e94a9fe/internal/pkg/monitoring/monitoring.go
package monitoring

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"contrib.go.opencensus.io/exporter/stackdriver"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/infra/build/siso/o11y/clog"
)

var (
	osFamilyKey       = tag.MustNewKey("os_family")
	versionKey        = tag.MustNewKey("siso_version")
	statusKey         = tag.MustNewKey("status")
	remoteStatusKey   = tag.MustNewKey("remote_status")
	exitCodeKey       = tag.MustNewKey("exit_code")
	remoteExitCodeKey = tag.MustNewKey("remote_exit_code")

	// actionCount is a metric for tracking the number of actions.
	actionCount = stats.Int64("rbe/action/count", "Number of actions processed", stats.UnitDimensionless)
	// actionLatency is a metric for tracking the e2e latency of an action.
	actionLatency = stats.Float64("rbe/action/latency", "Time spent processing an action", stats.UnitMilliseconds)
	// buildCacheHitRatio is a metric of the ratio of cache hits in a build.
	buildCacheHitRatio = stats.Float64("rbe/build/cache_hit_ratio", "Ratio of cache hits in a build", stats.UnitDimensionless)
	// buildLatency is a metric for tracking the e2e latency of a build.
	buildLatency = stats.Float64("rbe/build/latency", "E2e build time spent in Siso", stats.UnitSeconds)
	// buildCount is a metric for tracking the number of builds.
	buildCount = stats.Int64("rbe/build/count", "Counter for builds", stats.UnitDimensionless)

	// mu protects updating staticLabels.
	mu sync.Mutex
	// staticLabels are the labels for all metrics.
	staticLabels = make(map[tag.Key]string)
)

// SetupViews sets up monitoring views. This can only be run once.
func SetupViews(ctx context.Context, version string, labels map[string]string) error {
	if len(staticLabels) != 0 {
		return errors.New("views were already setup, cannot overwrite")
	}
	mu.Lock()
	defer mu.Unlock()

	staticLabels[osFamilyKey] = runtime.GOOS
	staticLabels[versionKey] = version

	keys := []tag.Key{osFamilyKey, versionKey}
	for l, v := range labels {
		k, err := tag.NewKey(l)
		if err != nil {
			return err
		}
		staticLabels[k] = v
		keys = append(keys, k)
	}
	clog.Infof(ctx, "static labels for monitoring were set. %v", staticLabels)
	views := []*view.View{
		{
			Measure:     actionLatency,
			TagKeys:     append(slices.Clone(keys), statusKey, remoteStatusKey, exitCodeKey, remoteExitCodeKey),
			Aggregation: view.Distribution(1, 2, 3, 4, 5, 6, 8, 10, 13, 16, 20, 25, 30, 40, 50, 65, 80, 100, 130, 160, 200, 250, 300, 400, 500, 650, 800, 1000, 2000, 5000, 10000, 20000, 50000, 100000, 200000, 500000),
		},
		{
			Measure:     actionCount,
			TagKeys:     append(slices.Clone(keys), statusKey, remoteStatusKey, exitCodeKey, remoteExitCodeKey),
			Aggregation: view.Sum(),
		},
		{
			Measure:     buildCacheHitRatio,
			TagKeys:     slices.Clone(keys),
			Aggregation: view.Distribution(0.05, 0.1, 0.15, 0.20, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1),
		},
		{
			Measure:     buildLatency,
			TagKeys:     slices.Clone(keys),
			Aggregation: view.Distribution(1, 10, 60, 120, 300, 600, 1200, 2400, 3000, 3600, 4200, 4800, 5400, 6000, 6600, 7200, 9000, 10800, 12600, 14400),
		},
		{
			Measure:     buildCount,
			TagKeys:     append(slices.Clone(keys), statusKey),
			Aggregation: view.Sum(),
		},
	}

	return view.Register(views...)
}

// NewExporter returns a new Cloud monitoring metrics exporter.
func NewExporter(ctx context.Context, project, prefix, rbeProject string, copts []option.ClientOption) (*stackdriver.Exporter, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	// Location is hard-coded in Reclient.
	// https://github.com/bazelbuild/reclient/blob/4d9d00de3f05c24ce2af03455243bed45e94a9fe/internal/pkg/monitoring/monitoring.go#L50C17-L50C30
	// TODO: Check if it's fine to set different locations, also non-GCE bots and workstations don't have GCE zone value.
	location := "us-central1-a"
	opts := stackdriver.Options{
		ProjectID:               project,
		MonitoringClientOptions: copts,
		OnError: func(err error) {
			switch status.Code(err) {
			case codes.Unavailable:
				clog.Warningf(ctx, "failed to export to Stackdriver: %v", err)
			default:
				clog.Errorf(ctx, "failed to export to Stackdriver: %v %v", status.Code(err), err)
			}
		},
		MetricPrefix:      prefix,
		ReportingInterval: time.Minute,
		MonitoredResource: genericNode{
			project:   project,
			namespace: rbeProject,
			location:  location,
			node:      hostname,
		},
		DefaultMonitoringLabels: &stackdriver.Labels{},
	}
	e, err := stackdriver.NewExporter(opts)
	if err != nil {
		return nil, err
	}
	if err = e.StartMetricsExporter(); err != nil {
		return nil, err
	}
	clog.Infof(ctx, "Stackdriver exporter has started in %q. metric_prefix=%q, generic node labels={project=%q, namespace=%q, location=%q, node=%q}", project, prefix, project, rbeProject, location, hostname)
	return e, nil
}

// genericNode implements https://pkg.go.dev/contrib.go.opencensus.io/exporter/stackdriver/monitoredresource#Interface
// See also https://cloud.google.com/monitoring/api/resources#tag_generic_node
type genericNode struct {
	project   string
	namespace string
	location  string
	node      string
}

func (mr genericNode) MonitoredResource() (resType string, labels map[string]string) {
	return "generic_node", map[string]string{
		"project_id": mr.project,
		"namespace":  mr.namespace,
		"location":   mr.location,
		"node_id":    mr.node,
	}
}

// ExportActionMetrics exports metrics for one log record to opencensus.
func ExportActionMetrics(ctx context.Context, latency time.Duration, ar, remoteAr *rpb.ActionResult, actionErr, remoteErr error, cached bool) {
	// Use the same status values with CommandResultStatus in remote-apis-sdks to be aligned with Reclient. e.g. SUCCESS, CACHE_HIT
	// See also CommandResultStatus in remote-apis-sdks.
	// https://github.com/bazelbuild/remote-apis-sdks/blob/f4821a2a072c44f9af83002cf7a272fff8223fa3/go/api/command/command.proto#L172
	// TODO: Support REMOTE_ERROR, LOCAL_ERROR types if necessary.
	exitCode := ar.GetExitCode()
	var st string
	switch {
	case cached:
		st = "CACHE_HIT"
	case status.Code(actionErr) == codes.DeadlineExceeded || errors.Is(actionErr, context.DeadlineExceeded):
		st = "TIMEOUT"
	case exitCode != 0:
		st = "NON_ZERO_EXIT"
	default:
		st = "SUCCESS"
	}

	remoteExitCode := remoteAr.GetExitCode()
	var remoteStatus string
	switch {
	case cached:
		remoteStatus = "CACHE_HIT"
	case status.Code(remoteErr) == codes.DeadlineExceeded || errors.Is(remoteErr, context.DeadlineExceeded):
		remoteStatus = "TIMEOUT"
	case remoteExitCode != 0:
		remoteStatus = "NON_ZERO_EXIT"
	default:
		remoteStatus = "SUCCESS"
	}
	actCtx := contextWithTags(contextWithTags(ctx, staticLabels), map[tag.Key]string{
		statusKey:         st,
		exitCodeKey:       strconv.FormatInt(int64(exitCode), 10),
		remoteStatusKey:   remoteStatus,
		remoteExitCodeKey: strconv.FormatInt(int64(remoteExitCode), 10),
	})
	stats.Record(actCtx, actionCount.M(1))
	stats.Record(actCtx, actionLatency.M(float64(latency)/1e6))
}

// ExportBuildMetrics exports overall build metrics to opencensus.
func ExportBuildMetrics(ctx context.Context, latency time.Duration, cacheHitRatio float64, isErr bool) {
	status := "SUCCESS"
	if isErr {
		status = "FAILURE"
	}
	buildCtx := contextWithTags(contextWithTags(ctx, staticLabels), map[tag.Key]string{
		statusKey: status,
	})
	stats.Record(buildCtx, buildCount.M(1))
	stats.Record(buildCtx, buildLatency.M(latency.Seconds()))
	stats.Record(buildCtx, buildCacheHitRatio.M(cacheHitRatio))
}

func contextWithTags(ctx context.Context, tags map[tag.Key]string) context.Context {
	var m []tag.Mutator
	kvs := ""
	for k, v := range tags {
		m = append(m, tag.Insert(k, v))
		kvs += fmt.Sprintf("%v=%v,", k.Name(), v)
	}
	newCtx, err := tag.New(ctx, m...)
	if err != nil {
		clog.Warningf(ctx, "failed to set tags %v: %v", kvs, err)
	}
	return newCtx
}
