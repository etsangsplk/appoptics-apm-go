// Copyright (C) 2017 Librato, Inc. All rights reserved.

package reporter

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/config"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/host"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/log"
)

// defines what methods a reporter should offer (internal to reporter package)
type reporter interface {
	// called when an event should be reported
	reportEvent(ctx *oboeContext, e *event) error
	// called when a status (e.g. __Init message) should be reported
	reportStatus(ctx *oboeContext, e *event) error
	// called when a Span message should be reported
	reportSpan(span SpanMessage) error
	// Shutdown closes the reporter.
	Shutdown(ctx context.Context) error
	// ShutdownNow closes the reporter immediately
	ShutdownNow() error
	// Closed returns if the reporter is already closed.
	Closed() bool
	// WaitForReady waits until the reporter becomes ready or the context is canceled.
	WaitForReady(context.Context) bool
}

// KVs from getSettingsResult arguments
const (
	kvBucketCapacity       = "BucketCapacity"
	kvBucketRate           = "BucketRate"
	kvMetricsFlushInterval = "MetricsFlushInterval"
	kvEventsFlushInterval  = "EventsFlushInterval"
	kvMaxTransactions      = "MaxTransactions"
)

// currently used reporter
var globalReporter reporter = &nullReporter{}

var (
	periodicTasksDisabled = false // disable periodic tasks, for testing
)

// a noop reporter
type nullReporter struct{}

func newNullReporter() *nullReporter                                  { return &nullReporter{} }
func (r *nullReporter) reportEvent(ctx *oboeContext, e *event) error  { return nil }
func (r *nullReporter) reportStatus(ctx *oboeContext, e *event) error { return nil }
func (r *nullReporter) reportSpan(span SpanMessage) error             { return nil }
func (r *nullReporter) Shutdown(ctx context.Context) error            { return nil }
func (r *nullReporter) ShutdownNow() error                            { return nil }
func (r *nullReporter) Closed() bool                                  { return true }
func (r *nullReporter) WaitForReady(ctx context.Context) bool         { return true }

// init() is called only once on program startup. Here we create the reporter
// that will be used throughout the runtime of the app. Default is 'ssl' but
// can be overridden via APPOPTICS_REPORTER
func init() {
	initReporter()
	sendInitMessage()
}

func initReporter() {
	r := config.GetReporterType()
	if config.GetDisabled() {
		r = "none"
		log.Warning("AppOptics reporter is disabled.")
	}
	setGlobalReporter(r)
}

func setGlobalReporter(reporterType string) {
	// Close the previous reporter
	if globalReporter != nil {
		globalReporter.ShutdownNow()
	}

	switch strings.ToLower(reporterType) {
	case "ssl":
		fallthrough // using fallthrough since the SSL reporter (gRPC) is our default reporter
	default:
		globalReporter = newGRPCReporter()
	case "udp":
		globalReporter = udpNewReporter()
	case "none":
		globalReporter = newNullReporter()
	}
}

// WaitForReady waits until the reporter becomes ready or the context is canceled.
func WaitForReady(ctx context.Context) bool {
	// globalReporter is not protected by a mutex as currently it's only modified
	// from the init() function.
	return globalReporter.WaitForReady(ctx)
}

// Shutdown flushes the metrics and stops the reporter. It blocked until the reporter
// is shutdown or the context is canceled.
func Shutdown(ctx context.Context) error {
	return globalReporter.Shutdown(ctx)
}

// Closed indicates if the reporter has been shutdown
func Closed() bool {
	return globalReporter.Closed()
}

// ReportSpan is called from the app when a span message is available
// span	span message to be put on the channel
//
// returns	error if channel is full
func ReportSpan(span SpanMessage) error {
	return globalReporter.reportSpan(span)
}

// check if context and event are valid, add general keys like Timestamp, or hostname
// ctx		oboe context
// e		event to be prepared for sending
//
// returns	error if invalid context or event
func prepareEvent(ctx *oboeContext, e *event) error {
	if ctx == nil || e == nil {
		return errors.New("invalid context, event")
	}

	// The context metadata must have the same task_id as the event.
	if !bytes.Equal(ctx.metadata.ids.taskID, e.metadata.ids.taskID) {
		return errors.New("invalid event, different task_id from context")
	}

	// The context metadata must have a different op_id than the event.
	if bytes.Equal(ctx.metadata.ids.opID, e.metadata.ids.opID) {
		return errors.New("invalid event, same as context")
	}

	us := time.Now().UnixNano() / 1000
	e.AddInt64("Timestamp_u", us)

	e.AddString("Hostname", host.Hostname())
	e.AddInt("PID", host.PID())

	// Update the context's op_id to that of the event
	ctx.metadata.ids.setOpID(e.metadata.ids.opID)

	bsonBufferFinish(&e.bbuf)
	return nil
}

// Determines if request should be traced, based on sample rate settings.
func shouldTraceRequest(layer string, traced bool) (bool, int, sampleSource) {
	return oboeSampleRequest(layer, traced)
}

func argsToMap(capacity, ratePerSec float64, metricsFlushInterval, maxTransactions int) map[string][]byte {
	args := make(map[string][]byte)

	if capacity > -1 {
		bits := math.Float64bits(capacity)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvBucketCapacity] = bytes
	}
	if ratePerSec > -1 {
		bits := math.Float64bits(ratePerSec)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvBucketRate] = bytes
	}
	if metricsFlushInterval > -1 {
		bytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(bytes, uint32(metricsFlushInterval))
		args[kvMetricsFlushInterval] = bytes
	}
	if maxTransactions > -1 {
		bytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(bytes, uint32(maxTransactions))
		args[kvMaxTransactions] = bytes
	}

	return args
}
