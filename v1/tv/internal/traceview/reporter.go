// Copyright (C) 2016 Librato, Inc. All rights reserved.

package traceview

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/librato/go-traceview/v1/tv/internal/traceview/collector"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io/ioutil"
	"strconv"
	"strings"
	"sync"
)

// Reporter status
const (
	OK           Status = iota
	DISCONNECTED        // will try to reconnect it later
	CLOSING             // closed by us, don't do anything to resume it
)

// The default reporter parameters which may be changed by the collector through settings retrieval.
const (
	maxEventBytes                 = 64 * 1024 * 1024
	grpcReporterFlushTimeout      = 100 * time.Millisecond
	agentMetricsInterval          = time.Minute
	agentMetricsTickInterval      = time.Millisecond * 500
	retryAmplifier                = 2
	initialRetryInterval          = time.Millisecond * 500
	maxRetryInterval              = time.Minute
	maxMetricsRetries             = 20
	maxConnRedirects              = 20
	maxConnRetries                = int(^uint(0) >> 1)
	maxStatusChanCap              = 200
	loadStatusMsgsShortBlock      = time.Millisecond * 5
	metricsConnKeepAliveInterval  = time.Second * 20
	maxMetricsMessagesOnePost     = 100
	agentSettingsInterval         = time.Second * 20
	agentCheckSettingsTTLInterval = time.Second * 10
)

type reporter interface {
	WritePacket([]byte) (int, error)
	IsOpen() bool
	IsMetricsConnOpen() bool
	// PushMetricsRecord is invoked by a trace to push the mAgg record
	PushMetricsRecord(record MetricsRecord) bool
}

func newUDPReporter() reporter {
	var conn *net.UDPConn
	if reportingDisabled {
		return &nullReporter{}
	}
	serverAddr, err := net.ResolveUDPAddr("udp4", udpReporterAddr)
	if err == nil {
		conn, err = net.DialUDP("udp4", nil, serverAddr)
	}
	if err != nil {
		if os.Getenv("TRACEVIEW_DEBUG") != "" {
			log.Printf("TraceView failed to initialize UDP reporter: %v", err)
		}
		return &nullReporter{}
	}
	return &udpReporter{conn: conn}
}

type nullReporter struct{}

func (r *nullReporter) IsOpen() bool                                { return false }
func (r *nullReporter) IsMetricsConnOpen() bool                     { return false }
func (r *nullReporter) WritePacket(buf []byte) (int, error)         { return len(buf), nil }
func (r *nullReporter) PushMetricsRecord(record MetricsRecord) bool { return true }

type udpReporter struct {
	conn *net.UDPConn
}

func (r *udpReporter) IsOpen() bool                                { return r.conn != nil }
func (r *udpReporter) IsMetricsConnOpen() bool                     { return false }
func (r *udpReporter) WritePacket(buf []byte) (int, error)         { return r.conn.Write(buf) }
func (r *udpReporter) PushMetricsRecord(record MetricsRecord) bool { return false }

type Status int

type Sender struct {
	messages       [][]byte
	nextTime       time.Time
	retryActive    bool
	nextRetryDelay time.Duration
	retryTime      time.Time
	retries        int
}

type gRPC struct {
	client            collector.TraceCollectorClient
	status            Status
	retries           int
	nextRetryTime     time.Time // only works in DISCONNECTED state
	redirects         int
	nextKeepAliveTime time.Time
	currTime          time.Time
}

type settings struct {
	maxEventBytes                 int
	grpcReporterFlushTimeout      time.Duration
	agentMetricsInterval          time.Duration
	agentMetricsTickInterval      time.Duration
	retryAmplifier                int
	initialRetryInterval          time.Duration
	maxRetryInterval              time.Duration
	maxMetricsRetries             int
	maxConnRedirects              int
	maxConnRetries                int
	maxStatusChanCap              int
	loadStatusMsgsShortBlock      time.Duration
	metricsConnKeepAliveInterval  time.Duration
	maxMetricsMessagesOnePost     int
	agentSettingsInterval         time.Duration
	agentCheckSettingsTTLInterval time.Duration
}

func newDefaultSettings() settings {
	return settings{
		maxEventBytes:                 maxEventBytes,
		grpcReporterFlushTimeout:      grpcReporterFlushTimeout,
		agentMetricsInterval:          agentMetricsInterval,
		agentMetricsTickInterval:      agentMetricsTickInterval,
		retryAmplifier:                retryAmplifier,
		initialRetryInterval:          initialRetryInterval,
		maxRetryInterval:              maxRetryInterval,
		maxMetricsRetries:             maxMetricsRetries,
		maxConnRedirects:              maxConnRedirects,
		maxConnRetries:                maxConnRetries,
		maxStatusChanCap:              maxStatusChanCap,
		loadStatusMsgsShortBlock:      loadStatusMsgsShortBlock,
		metricsConnKeepAliveInterval:  metricsConnKeepAliveInterval,
		maxMetricsMessagesOnePost:     maxMetricsMessagesOnePost,
		agentSettingsInterval:         agentSettingsInterval,
		agentCheckSettingsTTLInterval: agentCheckSettingsTTLInterval,
	}
}

type grpcReporter struct {
	client     collector.TraceCollectorClient
	serverAddr string // server address in string format: host:port
	certPath   string
	exit       chan struct{}
	apiKey     string
	s          settings

	metricsConn               gRPC // metrics sender connection, for metrics, status and settings
	metrics, status, settings Sender
	ch                        chan []byte       // event messages
	mAgg                      MetricsAggregator // metrics raw records, need pre-processing
	sMsgs                     chan []byte       // status messages
}

type grpcResult struct {
	result *collector.MessageResult
	err    error
}

func (s *Sender) setRetryDelay(now time.Time, retryAmplifier int, maxMetricsRetries int) bool {
	if s.retries >= maxMetricsRetries {
		OboeLog(WARNING, "Maximum number of retries reached", nil)
		s.retryActive = false
		return false
	}
	s.retryTime = now.Add(s.nextRetryDelay)
	OboeLog(DEBUG, fmt.Sprintf("Retry in %d seconds", s.nextRetryDelay/time.Second))
	s.retries += 1
	if !s.retryActive {
		s.retryActive = true
	}
	s.nextRetryDelay *= time.Duration(retryAmplifier)
	if s.nextRetryDelay > time.Minute {
		s.nextRetryDelay = time.Minute
	}
	return true
}

func (r *grpcReporter) IsOpen() bool            { return r.client != nil }
func (r *grpcReporter) IsMetricsConnOpen() bool { return r.metricsConn.client != nil }
func (r *grpcReporter) WritePacket(buf []byte) (int, error) {
	r.ch <- buf
	return len(buf), nil
}

func (r *grpcReporter) reportEvents() {
	// TODO: update reporterCounters in mAgg (numSent, numFailed, etc.) for MetricsMessage
	// TODO: e.g., r.mAgg.IncrementReporterCounter(); don't update mAgg in reportEvents
	batches := make(chan [][]byte)
	results := r.postEvents(batches)

	var batch [][]byte
	var eventBytes int
	var logIsRunning bool
	flushBatch := func() {
		if !logIsRunning && len(batch) > 0 {
			logIsRunning = true
			batches <- batch
			batch = nil
			eventBytes = 0
		}
	}
	for {
		select {
		case evbuf := <-r.ch:
			if (eventBytes + len(evbuf)) > r.s.maxEventBytes { // max buffer reached
				if len(evbuf) >= r.s.maxEventBytes {
					break // new event larger than max buffer size, drop
				}
				// drop oldest to make room for newest
				for dropped := 0; dropped < len(evbuf); {
					var oldest []byte
					oldest, batch = batch[0], batch[1:]
					dropped += len(oldest)
				}
			}
			// apend to batch
			batch = append(batch, evbuf)
			eventBytes += len(evbuf)
		case result := <-results:
			_ = result // XXX check return code, reconnect if disconnected
			logIsRunning = false
			flushBatch()
		case <-time.After(r.s.grpcReporterFlushTimeout):
			flushBatch()
		case <-r.exit:
			close(batches)
			break
		}
	}
}

func (r *grpcReporter) postEvents(batches <-chan [][]byte) <-chan *grpcResult {
	ret := make(chan *grpcResult)
	go func() {
		for batch := range batches {
			// call PostEvents
			req := &collector.MessageRequest{
				ApiKey:   r.apiKey,
				Messages: batch,
				Encoding: collector.EncodingType_BSON,
			}
			res, err := r.client.PostEvents(context.TODO(), req)
			ret <- &grpcResult{result: res, err: err}
		}
		close(ret)
	}()
	return ret
}

func (r *grpcReporter) PushMetricsRecord(record MetricsRecord) bool {
	if !r.IsMetricsConnOpen() {
		return false
	}
	return r.mAgg.PushMetricsRecord(&record)
}

// periodic is executed in a separate goroutine to encode messages and push them to the gRPC server
// This function is not concurrency-safe, don't run it in multiple goroutines.
func (r *grpcReporter) periodic() {
	OboeLog(DEBUG, "Goroutine started")
	go r.mAgg.ProcessMetrics()
	now := time.Now()
	// Initialize next keep alive time
	r.metricsConn.nextKeepAliveTime = getNextTime(now, r.s.metricsConnKeepAliveInterval)
	// Initialize next metric sending time
	r.metrics.nextTime = getNextTime(now, r.s.agentMetricsInterval)
	// Check and invalidate outdated settings
	var checkTTLTimeout = getNextTime(now, r.s.agentCheckSettingsTTLInterval)

	for {
		// avoid consuming too much CPU by sleeping for a short while.
		r.metricsConn.currTime = r.blockTillNextTick(time.Now(), r.s.agentMetricsTickInterval)
		// We still need to populate bson messages even if status is not OK.
		// populate and send metricsConn
		r.sendMetrics()
		// populate and send status
		r.sendStatus()
		// retrieve new settings
		r.getSettings()
		// invalidate outdated settings
		InvalidateOutdatedSettings(&checkTTLTimeout, r.metricsConn.currTime, r.s.agentCheckSettingsTTLInterval)
		// exit as per the request from the other (main) goroutine
		select {
		case <-r.exit:
			r.metricsConn.status = CLOSING
		default:
		}

		r.healthCheck()
		if r.metricsConnClosed() {
			// closed after health check, resources have been released.
			break // break the for loop
		}
	}
}

// metricsConnClosed checks if the metrics sending connection is closed
func (r *grpcReporter) metricsConnClosed() bool {
	return r.metricsConn.status == CLOSING && r.metricsConn.client == nil
}

// healthCheck checks the status of the reporter (e.g., gRPC connection) and try to fix
// any problems found. It tries to close the reporter and release resources if the
// reporter is request to close.
func (r *grpcReporter) healthCheck() {
	if r.metricsConn.status == OK {
		return
	}
	// Close the reporter if requested.
	if r.metricsConn.status == CLOSING {
		r.closeMetricsConn()
		return
	} else { // disconnected or reconnecting (check retry timeout)
		r.metricsConn.reconnect(r.serverAddr, r.certPath, r.s)
	}

}

// reconnect is used to reconnect to the grpc server when the status is DISCONNECTED
// Consider using mutex as multiple goroutines will access the status parallelly
func (g *gRPC) reconnect(addr string, certPath string, s settings) {
	// TODO: gRPC supports auto-reconnection, need to make sure what happens to the sending API then,
	// TODO: does it wait for the reconnection, or it returns an error immediately?
	if g.status == OK || g.status == CLOSING {
		return
	} else {
		if g.retries > s.maxConnRetries { // infinitely retry
			OboeLog(ERROR, fmt.Sprintf("Reached retries limit: %v, exiting", s.maxConnRetries))
			g.status = CLOSING // set it to CLOSING, it will be closed in the next loop
			return
		}

		if g.nextRetryTime.After(g.currTime) {
			// do nothing
		} else { // reconnect
			OboeLog(DEBUG, "Reconnecting to gRPC server")
			// TODO: close the old connection first, as we are redirecting ...

			conn, err := dialGRPC(certPath, addr)
			if err != nil {
				OboeLog(WARNING, fmt.Sprintf("Failed to reconnect gRPC reporter: %v %v", addr, err))
				// TODO: retry time better to be exponential
				nextInterval := time.Second * time.Duration((g.retries+1)*s.retryAmplifier)
				if nextInterval > s.maxRetryInterval {
					nextInterval = s.maxRetryInterval
				}
				g.nextRetryTime = g.currTime.Add(nextInterval) // TODO: round up?
				g.retries += 1
			} else { // reconnected
				g.client = collector.NewTraceCollectorClient(conn)
				g.retries = 0
				g.nextRetryTime = time.Time{}
				g.status = OK
				g.nextKeepAliveTime = getNextTime(g.currTime, s.metricsConnKeepAliveInterval)
			}

		}
	}
}

func dialGRPC(certPath string, addr string) (*grpc.ClientConn, error) {
	certPool := x509.NewCertPool()
	ca, err := ioutil.ReadFile(certPath)
	if err != nil {
		return nil, errors.New("No cert file found")
	}

	if ok := certPool.AppendCertsFromPEM(ca); !ok {
		return nil, errors.New("Unable to append the certificate to pool.")
	}

	creds := credentials.NewTLS(&tls.Config{
		ServerName:         addr,
		RootCAs:            certPool,
		InsecureSkipVerify: true, // TODO: a workaround, don't turn it on for production.
	})

	return grpc.Dial(addr, grpc.WithTransportCredentials(creds))
}

// Close request the reporter to quit from its goroutine by setting the exit flag
func (r *grpcReporter) RequestToClose() {
	r.exit <- struct{}{}
}

// close closes the channels and gRPC connections owned by a reporter
func (r *grpcReporter) closeMetricsConn() {
	if r.metricsConn.client == nil {
		OboeLog(WARNING, "Closing a closed connection.")
		return
	}
	// close channels and connections
	OboeLog(INFO, "Closing metrics gRPC connection.")
	// Finally set toe reporter to nil to avoid repeated closing
	close(r.mAgg.GetExitChan())

	// TODO: we should close the gRPC client but seems we don't have this method.
	r.metricsConn.client = nil
	r.metrics.messages = nil
	r.status.messages = nil
	r.settings.messages = nil
}

// blockTillNextTick blocks the caller and will return at the next wake up time, which
// is the nearest multiple of interval (since the zero time)
func (r *grpcReporter) blockTillNextTick(now time.Time, interval time.Duration) (curr time.Time) {
	// skip it if metricsConn connection is not working.
	if r.metricsConn.status != OK {
		return now
	}
	afterBlock := getNextTime(now, interval)
	<-time.After(afterBlock.Sub(now))
	return afterBlock
}

func getNextTime(now time.Time, interval time.Duration) time.Time {
	nextTime := now.Round(interval)
	if nextTime.Before(now) {
		nextTime = nextTime.Add(interval)
	}
	return nextTime
}

// sendMetrics is called periodically (in a interval defined by agentMetricsInterval)
// to send metricsConn data to the gRPC sercer
func (r *grpcReporter) sendMetrics() {
	// Still need to fetch raw data from channel to avoid channels being filled with old data
	// (and possibly blocks the sender)
	if r.metrics.nextTime.Before(r.metricsConn.currTime) {
		r.metrics.nextTime = getNextTime(r.metricsConn.currTime, r.s.agentMetricsInterval) // TODO: change to a value configured by settings.args

		message, err := r.mAgg.FlushBSON(r.s)
		if err == nil {
			r.metrics.messages = append(r.metrics.messages, message)
			if len(r.metrics.messages) > r.s.maxMetricsMessagesOnePost {
				r.metrics.messages = r.metrics.messages[1:]
			}
		}
	}
	// return if in retry state but it's not time for retry
	if r.metrics.retryActive && r.metrics.retryTime.After(r.metricsConn.currTime) {
		return
	}
	// return if connection is not OK or we have no message to send
	if r.metricsConn.status != OK || len(r.metrics.messages) == 0 {
		return
	}
	// OK we are good now.
	mreq := &collector.MessageRequest{
		ApiKey:   r.apiKey,
		Messages: r.metrics.messages,
		Encoding: collector.EncodingType_BSON,
	}
	mres, err := r.metricsConn.client.PostMetrics(context.TODO(), mreq)
	if err != nil {
		OboeLog(INFO, "Error in sending metrics", err)
		r.metricsConn.status = DISCONNECTED
		return
	}
	// Update connection keep alive time
	r.metricsConn.nextKeepAliveTime = getNextTime(r.metricsConn.currTime, r.s.metricsConnKeepAliveInterval)

	switch result := mres.GetResult(); result {
	case collector.ResultCode_OK:
		OboeLog(DEBUG, "Sent metrics.")
		r.metrics.messages = make([][]byte, 0, 1)
		r.metrics.retries = 0
		r.metrics.retryActive = false
		r.metricsConn.redirects = 0
	case collector.ResultCode_TRY_LATER, collector.ResultCode_LIMIT_EXCEEDED:
		msg := fmt.Sprintf("Got %s from server", collector.ResultCode_name[int32(result)])
		OboeLog(INFO, msg)
		if r.metrics.setRetryDelay(r.metricsConn.currTime, r.s.retryAmplifier, r.s.maxMetricsRetries) {
			r.metrics.messages = r.metrics.messages[1:] // TODO: correct?
		}
	case collector.ResultCode_INVALID_API_KEY:
		OboeLog(WARNING, "Got INVALID_API_KEY from server")
		r.metricsConn.status = CLOSING
		r.metrics.messages = nil // connection is closing so we're OK with nil
	case collector.ResultCode_REDIRECT:
		r.processRedirect(mres.GetArg())
	}
	return
}

// setServerAddr set the server address for grpcReporter as a string. It is not goroutine-safe
// as it is supposed to have only one goroutine to call it at any time.
func (r *grpcReporter) setServerAddr(host string) bool {
	if strings.Contains(host, ":") {
		OboeLog(WARNING, fmt.Sprintf("Invalid reporter server address: %s", host))
		return false
	} else {
		// we trust what we have got from the collector is a real/legitimate IP address
		r.serverAddr = host
		return true
	}

}

// TODO: need an API to the trace to send status message (check grpc is ready otherwise return)

// sendStatus is called periodically (in a interval defined by agentMetricsInterval)
// to send status events to the gRPC server.
func (r *grpcReporter) sendStatus() {
	if r.metricsConn.status != OK {
		return
	}
	// return if we're retrying and it's not time for retry
	if r.status.retryActive && r.status.retryTime.After(r.metricsConn.currTime) { // TODO: double check
		return
	}

	if len(r.status.messages) > 0 || r.loadStatusMsgs() {
		mreq := &collector.MessageRequest{
			ApiKey:   r.apiKey,
			Messages: r.status.messages,
			Encoding: collector.EncodingType_BSON,
		}
		mres, err := r.metricsConn.client.PostStatus(context.TODO(), mreq)
		if err != nil {
			OboeLog(INFO, "Error in sending metrics", err)
			r.metricsConn.status = DISCONNECTED
			return
		}
		// Update connection keep alive time
		r.metricsConn.nextKeepAliveTime = getNextTime(r.metricsConn.currTime, r.s.metricsConnKeepAliveInterval)

		switch result := mres.GetResult(); result {
		case collector.ResultCode_OK:
			OboeLog(DEBUG, "Sent status")
			r.status.messages = make([][]byte, 0, 1)
			r.status.retryActive = false
			r.metricsConn.redirects = 0
		case collector.ResultCode_TRY_LATER, collector.ResultCode_LIMIT_EXCEEDED:
			msg := fmt.Sprintf("Got %s from server", collector.ResultCode_name[int32(result)])
			OboeLog(INFO, msg)
			if r.status.setRetryDelay(r.metricsConn.currTime, r.s.retryAmplifier, r.s.maxMetricsRetries) {
				r.status.messages = make([][]byte, 0, 1)
			}
		case collector.ResultCode_INVALID_API_KEY:
			OboeLog(WARNING, "Got INVALID_API_KEY from server")
			r.metricsConn.status = CLOSING
			r.status.messages = nil // connection is closing so we're OK with nil
		case collector.ResultCode_REDIRECT:
			r.processRedirect(mres.GetArg())
		}
	}
}

// processRedirect process the redirect response from server and set the new server address
func (r *grpcReporter) processRedirect(host string) {
	if r.metricsConn.redirects >= r.s.maxConnRedirects {
		OboeLog(WARNING, "Maximum redirects reached, exiting")
		r.metricsConn.status = CLOSING
	} else {
		r.metricsConn.status = DISCONNECTED
		if r.setServerAddr(host) {
			r.metrics.retryActive = false
			r.metricsConn.redirects += 1
			r.metricsConn.retries = 0
			r.metricsConn.nextRetryTime = time.Time{}
		} else {
			r.metricsConn.status = CLOSING
		}
	}
}

// TODO: API for sending and encoding status message

// loadStatusMsgs loads messages from reporter's sMsgs channel to the status senders
// messages slice, the messages will be sent out in current loop
func (r *grpcReporter) loadStatusMsgs() bool {
	var sMsg []byte
loop:
	for {
		select {
		case sMsg = <-r.sMsgs:
			r.status.messages = append(r.status.messages, sMsg)
		case <-time.After(r.s.loadStatusMsgsShortBlock):
			break loop
		}
	}
	return len(r.status.messages) > 0
}

// getSettings is called periodically (in a interval defined by agentMetricsInterval)
// to retrieve updated setting from gRPC server and process it.
func (r *grpcReporter) getSettings() { // TODO: use it as keep alive msg
	if r.metricsConn.status != OK {
		return
	}

	tn := r.metricsConn.currTime
	if (!r.settings.retryActive &&
		(r.settings.nextTime.Before(tn) || r.metricsConn.nextKeepAliveTime.Before(tn))) ||
		(r.settings.retryActive && r.settings.retryTime.Before(tn)) {
		OboeLog(DEBUG, "Updating settings")
		var ipAddrs []string
		var uuid string

		mAgg, ok := r.mAgg.(*metricsAggregator)
		if ok {
			ipAddrs = mAgg.getIPList()
			uuid = mAgg.getHostId()
		} else {
			ipAddrs = nil
			uuid = ""
		}
		sreq := &collector.SettingsRequest{
			ApiKey:        r.apiKey,
			ClientVersion: grpcReporterVersion,
			Identity: &collector.HostID{
				Hostname:    cachedHostname,
				IpAddresses: ipAddrs,
				Uuid:        uuid,
			},
		}
		sres, err := r.metricsConn.client.GetSettings(context.TODO(), sreq)
		if err != nil {
			OboeLog(INFO, "Error in retrieving settings", err)
			r.metricsConn.status = DISCONNECTED
			return
		}
		r.metricsConn.nextKeepAliveTime = getNextTime(r.metricsConn.currTime, r.s.metricsConnKeepAliveInterval)

		switch result := sres.GetResult(); result {
		case collector.ResultCode_OK:
			OboeLog(DEBUG, "Got new settings from server")
			storeSettings(sres)
			r.settings.nextTime = getNextTime(r.metricsConn.currTime, r.s.agentSettingsInterval)
			r.settings.retryActive = false
			r.metricsConn.redirects = 0
		case collector.ResultCode_TRY_LATER, collector.ResultCode_LIMIT_EXCEEDED:
			msg := fmt.Sprintf("Got %s from server", collector.ResultCode_name[int32(result)])
			OboeLog(INFO, msg)
			r.settings.retries = 0 // retry infinitely
			r.settings.setRetryDelay(r.metricsConn.currTime, r.s.retryAmplifier, r.s.maxMetricsRetries)

		case collector.ResultCode_INVALID_API_KEY:
			OboeLog(DEBUG, "Got INVALID_API_KEY, exiting")
			r.metricsConn.status = CLOSING
		case collector.ResultCode_REDIRECT:
			r.processRedirect(sres.GetArg())
		}
	}
}

// TODO: update settings
func storeSettings(r *collector.SettingsResult) {
	if r != nil && len(r.Settings) > 0 {
		latestSettings = r.Settings
		// TODO: update r.settings, we may need channels to send req/get value to avoid settings mutex
		// TODO: as it may be accessed by multiple goroutines concurrently.
	}
}

// TODO:
func InvalidateOutdatedSettings(timeout *time.Time, curr time.Time, interval time.Duration) {
	if timeout.Before(curr) {
		// TODO: delete outdated settings
		*timeout = getNextTime(curr, interval)
	}
}

func newSender(initialRetryInterval time.Duration) Sender {
	return Sender{
		messages:       make([][]byte, 0, 1),
		nextTime:       time.Time{},
		retryActive:    false,
		nextRetryDelay: initialRetryInterval,
		retryTime:      time.Time{},
		retries:        0,
	}
}

func newGRPC(client collector.TraceCollectorClient) gRPC {
	return gRPC{
		client:            client,
		status:            OK,
		retries:           0,
		nextRetryTime:     time.Time{},
		redirects:         0,
		nextKeepAliveTime: time.Time{},
		currTime:          time.Time{},
	}
}

func newGRPCReporter() reporter {
	// TODO: fetch data and release channel space even when gRPC is disconnected
	// We don't have the chance to reenable it then.
	if reportingDisabled {
		return &nullReporter{}
	}
	var key string
	if key = os.Getenv("APPOPTICS_SERVICE_KEY"); key == "" {
		OboeLog(WARNING, "No service key found, check environment variable APPOPTICS_SERVICE_KEY.")
		return &nullReporter{}
	}

	var reporterAddr string
	if str := os.Getenv("APPOPTICS_COLLECTOR"); str != "" {
		reporterAddr = str
	} else { // else use the default one
		reporterAddr = grpcReporterAddr
	}
	certPath := os.Getenv("GRPC_CERT_PATH")
	conn, err := dialGRPC(certPath, reporterAddr)
	if err != nil {
		OboeLog(WARNING, fmt.Sprintf("AppOptics failed to initialize gRPC reporter: %v %v", reporterAddr, err))
		return &nullReporter{}
	}
	mConn, err := dialGRPC(certPath, reporterAddr)
	if err != nil {
		OboeLog(ERROR, fmt.Sprintf("AppOptics failed to intialize gRPC metrics reporter: %v %v", reporterAddr, err))
		conn.Close()
		return &nullReporter{}
	}
	return newGRPCReporterWithConfig(collector.NewTraceCollectorClient(conn), newDefaultSettings(),
		collector.NewTraceCollectorClient(mConn), reporterAddr, certPath, key)
}

// newGRPCReporterWithConfig creates a new gRPC reporter with provided config arguments
func newGRPCReporterWithConfig(eClient collector.TraceCollectorClient, s settings,
	mClient collector.TraceCollectorClient, reporterAddr string, certPath string, apiKey string) reporter {
	r := &grpcReporter{
		client:      eClient,
		serverAddr:  reporterAddr,
		certPath:    certPath,
		apiKey:      apiKey,
		metricsConn: newGRPC(mClient),
		metrics:     newSender(s.initialRetryInterval),
		status:      newSender(s.initialRetryInterval),
		settings:    newSender(s.initialRetryInterval),
		ch:          make(chan []byte),
		exit:        make(chan struct{}),
		mAgg:        newMetricsAggregator(),
		sMsgs:       make(chan []byte, s.maxStatusChanCap),
		s:           s,
	}
	go r.reportEvents()
	go r.periodic() // metricsConn sender goroutine
	return r
}

var udpReporterAddr = "127.0.0.1:7831"
var grpcReporterAddr = "collector.librato.com:443"
var grpcReporterVersion = "golang-v2"

// Don't access _globalReporter directly, use globalReporter() and setGlobalReporter() instead
var _globalReporter reporter = &nullReporter{}

// initGlobalReporterOnce is used to make sure the reporter is only initialized once for each process
var initGlobalReporterOnce sync.Once

// initGlobalReporterChan is used to block the threads/goroutines waiting for the initialization
var initGlobalReporterChan = make(chan struct{})

// reportingDisabled is used to disable the reporting
var reportingDisabled bool = false

var usingTestReporter bool
var cachedHostname string
var debugLog bool = true
var debugLevel DebugLevel = ERROR
var latestSettings []*collector.OboeSetting

// globalReporter returns the reporter of the current process, it will call initReporter if
// it's not yet done.
func globalReporter() reporter {
	initGlobalReporterOnce.Do(initReporter)
	// Only the first thread/goroutine enters initReporter, all others are blocked here
	// before the initialization is done.
	<-initGlobalReporterChan
	if reportingDisabled {
		return &nullReporter{}
	} else {
		return _globalReporter
	}
}

// initReporter initializes the event and metrics reporters. This function should be called
// only once, which is usually invoked by sync.Once.Do()
func initReporter() {
	// close this channel after initialization is done, other goroutines/threads will be
	// released and able to get the initialized reporter.
	defer close(initGlobalReporterChan)

	if l := os.Getenv("APPOPTICS_DEBUG_LEVEL"); l != "" {
		if i, err := strconv.Atoi(l); err == nil {
			debugLevel = DebugLevel(i)
		} else {
			OboeLog(WARNING, "The debug level should be an integer.")
		}
	} else {
		debugLevel = ERROR
	}

	rType := strings.ToLower(os.Getenv("APPOPTICS_REPORTER"))
	if rType == "udp" {
		_globalReporter = newUDPReporter()
	} else {
		_globalReporter = newGRPCReporter()
	}

	if _, ok := _globalReporter.(*nullReporter); !ok {
		reportingDisabled = true
	} else {
		reportingDisabled = false
	}
}

type hostnamer interface {
	Hostname() (name string, err error)
}
type osHostnamer struct{}

func (h osHostnamer) Hostname() (string, error) { return os.Hostname() }

func init() {
	//debugLog = (os.Getenv("TRACEVIEW_DEBUG") != "")
	//if addr := os.Getenv("TRACEVIEW_GRPC_COLLECTOR_ADDR"); addr != "" {
	//	grpcReporterAddr = addr
	//}
	cacheHostname(osHostnamer{})
}
func cacheHostname(hn hostnamer) {
	h, err := hn.Hostname()
	if err != nil {
		if debugLog {
			log.Printf("Unable to get hostname, TraceView tracing disabled: %v", err)
		}
		reportingDisabled = true
	}
	cachedHostname = h
}

var cachedPid = os.Getpid()

func reportEvent(r reporter, ctx *oboeContext, e *event) error {
	if !r.IsOpen() {
		// Reporter didn't initialize, nothing to do...
		return nil
	}
	if ctx == nil || e == nil {
		return errors.New("Invalid context, event")
	}

	// The context metadata must have the same task_id as the event.
	if !bytes.Equal(ctx.metadata.ids.taskID, e.metadata.ids.taskID) {
		return errors.New("Invalid event, different task_id from context")
	}

	// The context metadata must have a different op_id than the event.
	if bytes.Equal(ctx.metadata.ids.opID, e.metadata.ids.opID) {
		return errors.New("Invalid event, same as context")
	}

	us := time.Now().UnixNano() / 1000
	e.AddInt64("Timestamp_u", us)

	// Add cached syscalls for Hostname & PID
	e.AddString("Hostname", cachedHostname)
	e.AddInt("PID", cachedPid)

	// Update the context's op_id to that of the event
	ctx.metadata.ids.setOpID(e.metadata.ids.opID)

	// Send BSON:
	bsonBufferFinish(&e.bbuf)
	_, err := r.WritePacket(e.bbuf.buf)
	return err
}

// Determines if request should be traced, based on sample rate settings:
// This is our only dependency on the liboboe C library.
func shouldTraceRequest(layer, xtraceHeader string) (sampled bool, sampleRate, sampleSource int) {
	return oboeSampleRequest(layer, xtraceHeader)
}

// PushMetricsRecord push the mAgg record into a channel using the global reporter.
func PushMetricsRecord(record MetricsRecord) bool {
	return globalReporter().PushMetricsRecord(record)
}
