/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/internal/cloudrun"
	"cloud.google.com/go/compute/metadata"
	"github.com/chainguard-dev/clog"
)

// regionLookupTimeout bounds one metadata lookup. The metadata server answers
// in milliseconds on GCP; the bound matters only when it is unreachable.
const regionLookupTimeout = 2 * time.Second

// regionRetryInterval spaces lookups after one fails, and regionLookupAttempts
// caps them, so a process that cannot reach its metadata server stops paying
// the timeout.
const (
	regionRetryInterval  = time.Minute
	regionLookupAttempts = 3
)

// dmiProductNamePath names the machine's product on Linux. GCE VMs, and the
// GKE nodes built on them, report "Google Compute Engine".
const dmiProductNamePath = "/sys/class/dmi/id/product_name"

// callLatencyEnv names the variable that turns RecordCallLatency off for the
// process. It is read from the environment, as DefaultResourceLabels reads
// its defaults, because it is an operator's safety valve: a recorder whose
// table lacks the latency columns and rejects unknown keys fails whole load
// batches, and the valve has to work in every service without each one wiring
// an option.
const callLatencyEnv = "DRIFTLESSAF_TRACE_CALL_LATENCY"

// callLatency is the process's switch for RecordCallLatency.
var callLatency = &latencySwitch{getenv: os.Getenv}

// latencySwitch reports whether the process records call latency. It reads
// callLatencyEnv the first time it is asked and keeps the answer for the life
// of the process. Unset or empty leaves recording on, and a value
// strconv.ParseBool reads as false turns it off. Any other value also turns it
// off: losing the latency fields costs less than a rejected load batch. Safe
// for concurrent use.
type latencySwitch struct {
	getenv func(string) string

	once sync.Once
	on   bool
}

func (s *latencySwitch) enabled(ctx context.Context) bool {
	s.once.Do(func() {
		value := strings.TrimSpace(s.getenv(callLatencyEnv))
		on, err := strconv.ParseBool(value)
		s.on = value == "" || (err == nil && on)
		if !s.on {
			clog.InfoContext(ctx, "Call latency recording is off", "variable", callLatencyEnv)
		}
	})
	return s.on
}

// clientRegion is the process's Region, read from the GCE metadata server.
var clientRegion = &regionCache{
	onGCP: func() bool {
		return onGCP(cloudrun.ServiceName(), os.Getenv("GCE_METADATA_HOST"), readDMIProductName())
	},
	lookup: func(ctx context.Context) (string, error) { return lookupRegion(ctx, metadata.GetWithContext) },
	now:    time.Now,
}

// regionCache returns the cloud Region this process runs in, such as
// "us-central1", or "" while it is unknown. It caches a found Region, and the
// absence of one off GCP, where it does no network I/O. A lookup that fails on
// GCP is logged and retried on a later call, no sooner than
// regionRetryInterval after it and at most regionLookupAttempts times in all.
// Safe for concurrent use; concurrent callers wait for an attempt in progress.
type regionCache struct {
	onGCP  func() bool
	lookup func(context.Context) (string, error)
	now    func() time.Time

	mu       sync.Mutex
	region   string
	settled  bool
	attempts int
	next     time.Time
}

func (c *regionCache) get(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settled || c.now().Before(c.next) {
		return c.region
	}
	if !c.onGCP() {
		c.settled = true
		return ""
	}
	// Bound the lookup on its own clock: a caller's cancelled request must not
	// spend an attempt.
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), regionLookupTimeout)
	defer cancel()
	region, err := c.lookup(lookupCtx)
	c.attempts++
	if err == nil {
		c.region, c.settled = region, true
		return region
	}
	c.settled = c.attempts >= regionLookupAttempts
	c.next = c.now().Add(regionRetryInterval)
	clog.WarnContext(ctx, "Could not read the client region from the metadata server",
		"attempt", c.attempts, "max_attempts", regionLookupAttempts, "reason", lookupFailure(err))
	return ""
}

// lookupFailure classifies a failed lookup. The error's own text carries the
// metadata URL, so it is not logged.
func lookupFailure(err error) string {
	if _, ok := errors.AsType[metadata.NotDefinedError](err); ok {
		return "not defined"
	}
	if ne, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || (ok && ne.Timeout()) {
		return "timeout"
	}
	return "request failed"
}

// RecordCallLatency records the timing of the HTTP attempt that returned a
// model response onto turn, and the process's client Region onto trace. It is
// best effort: a value the trace rejects is logged and dropped, and never fails
// the run. With DRIFTLESSAF_TRACE_CALL_LATENCY=false it records neither and
// does not look the Region up.
func RecordCallLatency[T any](ctx context.Context, trace *agenttrace.Trace[T], turn *agenttrace.LLMTurn[T], latency agenttrace.TurnLatency) {
	recordIfEnabled(ctx, callLatency, clientRegion, trace, turn, latency)
}

func recordIfEnabled[T any](ctx context.Context, sw *latencySwitch, regions *regionCache, trace *agenttrace.Trace[T], turn *agenttrace.LLMTurn[T], latency agenttrace.TurnLatency) {
	if !sw.enabled(ctx) {
		return
	}
	recordCallLatency(ctx, trace, turn, latency, regions.get(ctx))
}

func recordCallLatency[T any](ctx context.Context, trace *agenttrace.Trace[T], turn *agenttrace.LLMTurn[T], latency agenttrace.TurnLatency, region string) {
	if err := turn.RecordLatency(latency); err != nil {
		clog.WarnContext(ctx, "Dropped model call latency", "error", err)
	}
	// An empty region would clear one a caller's tracer already stamped.
	if region == "" {
		return
	}
	if err := trace.RecordClientRegion(region); err != nil {
		clog.WarnContext(ctx, "Dropped client region", "error", err)
	}
}

// onGCP reports whether the environment indicates GCP without contacting the
// network: a Cloud Run service or job name, an explicit metadata host, or a
// GCE product name.
func onGCP(cloudRunService, metadataHost, dmiProductName string) bool {
	if cloudRunService != "" || metadataHost != "" {
		return true
	}
	switch dmiProductName {
	case "Google", "Google Compute Engine":
		return true
	default:
		return false
	}
}

// readDMIProductName returns the machine's product name, or "" where the
// platform does not expose one.
func readDMIProductName() string {
	b, err := os.ReadFile(dmiProductNamePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// lookupRegion reads the Region from the metadata server. Cloud Run serves
// instance/region as "projects/<number>/regions/<region>". GCE and GKE serve
// only instance/zone, "projects/<number>/zones/<region>-<letter>", so the
// Region is the zone without its last hyphenated part.
func lookupRegion(ctx context.Context, get func(context.Context, string) (string, error)) (string, error) {
	region, regionErr := get(ctx, "instance/region")
	if regionErr == nil {
		return lastPathSegment(region), nil
	}
	zone, zoneErr := get(ctx, "instance/zone")
	if zoneErr != nil {
		return "", errors.Join(regionErr, zoneErr)
	}
	zone = lastPathSegment(zone)
	i := strings.LastIndexByte(zone, '-')
	if i <= 0 {
		return "", fmt.Errorf("zone %q has no region prefix", zone)
	}
	return zone[:i], nil
}

func lastPathSegment(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndexByte(s, '/')+1:]
}
