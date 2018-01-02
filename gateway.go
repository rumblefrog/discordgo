// Discordgo - Discord bindings for Go
// Available at https://github.com/bwmarrin/discordgo

// Copyright 2015-2016 Bruce Marriner <bruce@sqls.net>.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains low level functions for interacting with the Discord
// data websocket interface.

package discordgo

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"io"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyOpen = errors.New("Connection already open")
)

// GatewayOP represents a gateway operation
// see https://discordapp.com/developers/docs/topics/gateway#gateway-opcodespayloads-gateway-opcodes
type GatewayOP int

const (
	GatewayOPDispatch            GatewayOP = 0  // (Receive)
	GatewayOPHeartbeat           GatewayOP = 1  // (Send/Receive)
	GatewayOPIdentify            GatewayOP = 2  // (Send)
	GatewayOPStatusUpdate        GatewayOP = 3  // (Send)
	GatewayOPVoiceStateUpdate    GatewayOP = 4  // (Send)
	GatewayOPVoiceServerPing     GatewayOP = 5  // (Send)
	GatewayOPResume              GatewayOP = 6  // (Send)
	GatewayOPReconnect           GatewayOP = 7  // (Receive)
	GatewayOPRequestGuildMembers GatewayOP = 8  // (Send)
	GatewayOPInvalidSession      GatewayOP = 9  // (Receive)
	GatewayOPHello               GatewayOP = 10 // (Receive)
	GatewayOPHeartbeatACK        GatewayOP = 11 // (Receive)
)

type GatewayStatus int

const (
	GatewayStatusConnecting GatewayStatus = iota
	GatewayStatusIdentifying
	GatewayStatusReconnecting
	GatewayStatusNormal
)

// GatewayConnectionManager is responsible for managing the gateway connections for a single shard
// We create a new GatewayConnection every time we reconnect to avoid a lot of synchronization needs
// and also to avoid having to manually reset the connection state, all the workers related to the old connection
// should eventually stop, and if they're late they will be working on a closed connection anyways so it dosen't matter
type GatewayConnectionManager struct {
	mu sync.RWMutex

	// stores sessions current Discord Gateway
	gateway string

	shardCount int
	shardID    int

	session           *Session
	currentConnection *GatewayConnection
	status            GatewayStatus

	sessionID string
	sequence  int64

	idCounter int
}

// Open is a helper for Session.GatewayConnectionManager.Open(s.Gateway())
func (s *Session) Open() error {
	return s.GatewayManager.Open()
}

func (g *GatewayConnectionManager) Open() error {
	g.mu.Lock()
	if g.currentConnection != nil {
		g.mu.Unlock()
		g.currentConnection.Close()
		g.mu.Lock()
	}

	g.idCounter++

	if g.gateway == "" {
		gatewayAddr, err := g.session.Gateway()
		if err != nil {
			g.mu.Unlock()
			return err
		}
		g.gateway = gatewayAddr
	}

	g.initSharding()
	g.currentConnection = NewGatewayConnection(g, g.idCounter)

	sessionID := g.sessionID
	sequence := g.sequence

	g.mu.Unlock()

	return g.currentConnection.Open(sessionID, sequence)
}

// initSharding sets the sharding details and verifies that they are valid
func (g *GatewayConnectionManager) initSharding() {
	g.shardCount = g.session.ShardCount
	if g.shardCount < 1 {
		g.shardCount = 1
	}

	g.shardID = g.session.ShardID
	if g.shardID >= g.shardCount || g.shardID < 0 {
		g.mu.Unlock()
		panic("Invalid shardID: ID:" + strconv.Itoa(g.shardID) + " Count:" + strconv.Itoa(g.shardCount))
	}
}

// Close maintains backwards compatibility with old discordgo versions
func (s *Session) Close() error {
	return s.GatewayManager.Close()
}

func (g *GatewayConnectionManager) Close() (err error) {
	g.mu.Lock()
	if g.currentConnection != nil {
		g.mu.Unlock()
		err = g.currentConnection.Close()
		g.mu.Lock()
		g.currentConnection = nil
	}
	g.mu.Unlock()
	return
}

func (g *GatewayConnectionManager) Reconnect(forceIdentify bool) error {
	g.mu.RLock()
	currentConn := g.currentConnection
	g.mu.RUnlock()

	if currentConn != nil {
		err := g.currentConnection.Reconnect(forceIdentify)
		return err
	}

	return nil
}

type GatewayConnection struct {
	mu sync.Mutex

	// The parent manager
	manager *GatewayConnectionManager

	open         bool
	reconnecting bool
	status       GatewayStatus

	// Stores a mapping of guild id's to VoiceConnections
	voiceConnections map[string]*VoiceConnection

	// The underlying websocket connection.
	conn net.Conn

	// stores session ID of current Gateway connection
	sessionID string

	// This gets closed when the connection closes to signal all workers to stop
	stopWorkers chan interface{}

	wsReader *wsutil.Reader

	// contains the raw message fragments until we have received them all
	readMessageBuffer *bytes.Buffer

	zlibReader  io.Reader
	jsonDecoder *json.Decoder

	heartbeater *wsHeartBeater
	writer      *wsWriter

	connID int // A increasing id per connection from the connection manager to help identify the origin of logs
}

func NewGatewayConnection(parent *GatewayConnectionManager, id int) *GatewayConnection {
	return &GatewayConnection{
		manager:           parent,
		stopWorkers:       make(chan interface{}),
		readMessageBuffer: bytes.NewBuffer(make([]byte, 0, 0xffff)), // initial 65k buffer
		connID:            id,
	}
}

// Reconnect is a helper for Close() and Connect() and will attempt to resume if possible
func (g *GatewayConnection) Reconnect(forceReIdentify bool) error {
	g.log(LogInformational, "Reconnecting to the gateway")

	g.mu.Lock()
	if g.reconnecting {
		g.mu.Unlock()
		return nil
	}

	g.reconnecting = true

	if forceReIdentify {
		g.sessionID = ""
	}
	g.mu.Unlock()

	err := g.Close()
	if err != nil {
		return err
	}

	return g.manager.Open()
}

// ReconnectUnlessStopped will not reconnect if close was called earlier
func (g *GatewayConnection) ReconnectUnlessClosed(forceReIdentify bool) error {
	g.mu.Lock()
	if g.open {
		g.mu.Unlock()
		return g.Reconnect(forceReIdentify)
	}
	g.mu.Unlock()

	return nil
}

// Close closes the gateway connection
func (g *GatewayConnection) Close() error {
	g.log(LogInformational, "Closing gateway connection gateway")

	g.mu.Lock()

	// Only close the workers once
	wasOpen := false
	if g.open {
		close(g.stopWorkers)
		wasOpen = true
	}

	// If were not actually connected then do nothing
	g.open = false
	if g.conn == nil {
		g.mu.Unlock()
		return nil
	}

	// copy these here to later be assigned to the manager for possible resuming
	sidCop := g.sessionID
	seqCop := atomic.LoadInt64(g.heartbeater.sequence)

	g.mu.Unlock()

	if wasOpen {
		// Send the close frame
		g.writer.QueueClose(ws.StatusNormalClosure)

		// Wait for discord to close connnection
		time.Sleep(time.Second)

		g.manager.mu.Lock()
		g.manager.sessionID = sidCop
		g.manager.sequence = seqCop
		g.manager.mu.Unlock()
	}

	return nil
}

func (g *GatewayConnection) SessionState() {

}

// Connect connects to the discord gateway and starts handling frames
func (g *GatewayConnection) Open(sessionID string, sequence int64) error {
	g.mu.Lock()
	if g.open {
		g.mu.Unlock()
		return ErrAlreadyOpen
	}
	g.open = true

	conn, _, _, err := ws.Dial(context.TODO(), g.manager.gateway+"?v="+APIVersion+"&encoding=json&compress=zlib-stream")
	if err != nil {
		g.mu.Unlock()
		if conn != nil {
			conn.Close()
		}
		return err
	}

	g.conn = conn

	g.wsReader = wsutil.NewClientSideReader(conn)

	g.log(LogInformational, "Connected to the gateway websocket")

	err = g.startWorkers()
	if err != nil {
		return err
	}

	g.sessionID = sessionID

	g.mu.Unlock()

	if sessionID == "" {
		return g.identify()
	} else {
		g.heartbeater.UpdateSequence(sequence)
		return g.resume(sessionID, sequence)
	}
}

// startWorkers starts the background workers for reading, receiving and heartbeating
func (g *GatewayConnection) startWorkers() error {
	// Start the event reader
	go g.reader()

	// The writer
	writerWorker := &wsWriter{
		conn:           g.conn,
		session:        g.manager.session,
		closer:         g.stopWorkers,
		incoming:       make(chan interface{}),
		sendCloseQueue: make(chan ws.StatusCode),
	}
	g.writer = writerWorker
	go writerWorker.Run()

	// The heartbeater, this is started after we receive hello
	g.heartbeater = &wsHeartBeater{
		stop:        g.stopWorkers,
		writer:      g.writer,
		receivedAck: true,
		sequence:    new(int64),
		onNoAck: func() {
			g.log(LogError, "No heartbeat ack received since sending last heartbeast, reconnecting...")
			err := g.ReconnectUnlessClosed(false)
			if err != nil {
				g.log(LogError, "Failed reconnecting to the gateway: %v", err)
			}
		},
	}

	return nil
}

// reader reads incmoing messages from the gateway
func (g *GatewayConnection) reader() {

	// The buffer that is used to read into the bytes buffer
	// We need to control the amount read so we can't use buffer.ReadFrom directly
	// TODO: write it directly into the bytes buffer somehow to avoid a uneeded copy?
	intermediateBuffer := make([]byte, 0xffff)

	for {
		header, err := g.wsReader.NextFrame()
		if err != nil {
			g.onError(err, "error reading the next websocket frame")
			return
		}

		if !header.Fin {
			panic("welp should handle this")
		}

		for readAmount := int64(0); readAmount < header.Length; {

			n, err := g.wsReader.Read(intermediateBuffer)
			g.readMessageBuffer.Write(intermediateBuffer[:n])
			if err != nil {
				g.onError(err, "error reading the next websocket frame into intermediate buffer")
				return
			}

			readAmount += int64(n)
		}

		g.handleReadFrame(header)
	}
}

func (g *GatewayConnection) handleReadFrame(header ws.Header) {
	if header.OpCode == ws.OpClose {
		g.handleCloseFrame(g.readMessageBuffer.Bytes())
		g.readMessageBuffer.Reset()
		return
	}

	// TODO: Handle these properly
	if header.OpCode != ws.OpText && header.OpCode != ws.OpBinary {
		g.readMessageBuffer.Reset()
		g.log(LogError, "Don't know how to respond to websocket frame type: 0x%x", header.OpCode)
		return
	}

	// Package is not long enough to keep the enc of message suffix, so we need to wait for more data
	if g.readMessageBuffer.Len() < 4 {
		return
	}

	// Check if it's the end of the message, the frame should have the suffix 0x00 0x00 0xff 0xff
	raw := g.readMessageBuffer.Bytes()
	tail := raw[len(raw)-4:]

	if tail[0] != 0 || tail[1] != 0 || tail[2] != 0xff || tail[3] != 0xff {
		// Not the end of the packet
		return
	}

	g.handleReadMessage()
	g.readMessageBuffer.Reset()
}

func (g *GatewayConnection) handleCloseFrame(data []byte) {
	code := binary.BigEndian.Uint16(data)
	var msg string
	if len(data) > 2 {
		msg = string(data[2:])
	}

	g.log(LogError, "Got close frame, code: %d, Msg: %q", code, msg)
	err := g.ReconnectUnlessClosed(false)
	if err != nil {
		g.log(LogError, "Failed reconnecting to the gateway: %v", err)
	}
}

// handleReadMessage is called when we have received a full message
// it decodes the message into an event using a shared zlib context
func (g *GatewayConnection) handleReadMessage() {

	if g.zlibReader == nil {
		// We initialize the zlib reader here as opposed to in NewGatewayConntection because
		// zlib.NewReader apperently needs the header straight away, or it will block forever
		zr, err := zlib.NewReader(g.readMessageBuffer)
		if err != nil {
			g.onError(err, "failed creating zlib reader")
			return

		}

		g.zlibReader = zr
		g.jsonDecoder = json.NewDecoder(zr)
	}

	var event *Event
	err := g.jsonDecoder.Decode(&event)
	if err != nil {
		g.onError(err, "failed decoding incoming gateway event")
		return
	}

	g.handleEvent(event)
}

// handleEvent handles a event received from the reader
func (g *GatewayConnection) handleEvent(event *Event) {
	g.heartbeater.UpdateSequence(event.Sequence)

	var err error

	switch event.Operation {
	case GatewayOPDispatch:
		err = g.handleDispatch(event)
	case GatewayOPHeartbeat:
		g.log(LogInformational, "sending heartbeat immediately in response to OP1")
		g.heartbeater.SendBeat()
	case GatewayOPReconnect:
		g.log(LogWarning, "got OP7 reconnect, re-connecting.")
		err = g.Reconnect(false)
	case GatewayOPInvalidSession:
		g.log(LogWarning, "got OP2 invalid session, re-connecting.")

		time.Sleep(time.Second * time.Duration(rand.Intn(4)+1))

		if len(event.RawData) == 4 {
			// d == true, we can resume
			err = g.Reconnect(false)
		} else {
			err = g.Reconnect(true)
		}
	case GatewayOPHello:
		err = g.handleHello(event)
	case GatewayOPHeartbeatACK:
		g.heartbeater.ReceivedAck()
	default:
		g.log(LogWarning, "unknown operation (%d, %q): ", event.Operation, event.Type, string(event.RawData))
	}

	if err != nil {

		g.log(LogError, "error handling event (%d, %q)", event.Operation, event.Type)
	}
}

func (g *GatewayConnection) handleHello(event *Event) error {
	var h helloData
	err := json.Unmarshal(event.RawData, &h)
	if err != nil {
		return err
	}

	g.log(LogInformational, "receivied hello, heartbeat_interval: %d, _trace: %v", h.HeartbeatInterval, h.Trace)

	go g.heartbeater.Run(time.Duration(h.HeartbeatInterval) * time.Millisecond)

	return nil
}

func (g *GatewayConnection) handleDispatch(e *Event) error {

	// Map event to registered event handlers and pass it along to any registered handlers.
	if eh, ok := registeredInterfaceProviders[e.Type]; ok {
		e.Struct = eh.New()

		// Attempt to unmarshal our event.
		if err := json.Unmarshal(e.RawData, e.Struct); err != nil {
			g.log(LogError, "error unmarshalling %s event, %s", e.Type, err)
		}

		if rdy, ok := e.Struct.(*Ready); ok {
			g.handleReady(rdy)
		}

		// Send event to any registered event handlers for it's type.
		// Because the above doesn't cancel this, in case of an error
		// the struct could be partially populated or at default values.
		// However, most errors are due to a single field and I feel
		// it's better to pass along what we received than nothing at all.
		// TODO: Think about that decision :)
		// Either way, READY events must fire, even with errors.
		g.manager.session.handleEvent(e.Type, e.Struct)
	} else {
		g.log(LogWarning, "unknown event: Op: %d, Seq: %d, Type: %s, Data: %s", e.Operation, e.Sequence, e.Type, string(e.RawData))
	}

	// For legacy reasons, we send the raw event also, this could be useful for handling unknown events.
	g.manager.session.handleEvent(eventEventType, e)

	return nil
}

func (g *GatewayConnection) handleReady(r *Ready) {
	g.mu.Lock()
	g.sessionID = r.SessionID
	g.mu.Unlock()
}

func (g *GatewayConnection) identify() error {
	properties := identifyProperties{
		OS:              runtime.GOOS,
		Browser:         "Discordgo v" + VERSION,
		Device:          "",
		Referer:         "",
		ReferringDomain: "",
	}

	data := identifyData{
		Token:          g.manager.session.Token,
		Properties:     properties,
		LargeThreshold: 250,
		// Compress:      g.manager.session.Compress, // this is no longer needed since we use zlib-steam anyways
		Shard: nil,
	}

	if g.manager.shardCount > 1 {
		data.Shard = &[2]int{g.manager.shardID, g.manager.shardCount}
	}

	op := outgoingEvent{
		Operation: 2,
		Data:      data,
	}

	g.log(LogInformational, "Sending identify")

	g.writer.Queue(op)
	return nil
}

func (g *GatewayConnection) resume(sessionID string, sequence int64) error {
	op := &outgoingEvent{
		Operation: GatewayOPResume,
		Data: &resumeData{
			Token:     g.manager.session.Token,
			SessionID: sessionID,
			Sequence:  sequence,
		},
	}

	g.log(LogInformational, "Sending resume")

	g.writer.Queue(op)

	return nil
}

func (g *GatewayConnection) onError(err error, msgf string, args ...interface{}) {
	g.log(LogError, "%s: %s", fmt.Sprintf(msgf, args...), err.Error())
	if err := g.ReconnectUnlessClosed(false); err != nil {
		g.onError(err, "Failed reconnecting to the gateway")
	}
}

func (g *GatewayConnection) log(msgL int, msgf string, args ...interface{}) {
	if msgL > g.manager.session.LogLevel {
		return
	}

	prefix := fmt.Sprintf("[S%d:CID%d]: ", g.manager.shardID, g.connID)
	msglog(msgL, 2, prefix+msgf, args...)
}

type outgoingEvent struct {
	Operation GatewayOP   `json:"op"`
	Type      string      `json:"t,omitempty"`
	Data      interface{} `json:"d,omitempty"`
}

type identifyData struct {
	Token          string             `json:"token"`
	Properties     identifyProperties `json:"properties"`
	LargeThreshold int                `json:"large_threshold"`
	Compress       bool               `json:"compress"`
	Shard          *[2]int            `json:"shard,omitempty"`
}

type identifyProperties struct {
	OS              string `json:"$os"`
	Browser         string `json:"$browser"`
	Device          string `json:"$device"`
	Referer         string `json:"$referer"`
	ReferringDomain string `json:"$referring_domain"`
}

type helloData struct {
	HeartbeatInterval int64    `json:"heartbeat_interval"` // the interval (in milliseconds) the client should heartbeat with
	Trace             []string `json:"_trace"`
}

type resumeData struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Sequence  int64  `json:"seq"`
}
