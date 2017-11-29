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
	"net"
	"runtime"
	"strconv"
	"sync"
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
type GatewayConnectionManager struct {
	mu sync.RWMutex

	// stores sessions current Discord Gateway
	gateway string

	shardCount int
	shardID    int

	session           *Session
	currentConnection *GatewayConnection
	status            GatewayStatus
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

	if g.gateway == "" {
		gatewayAddr, err := g.session.Gateway()
		if err != nil {
			g.mu.Unlock()
			return err
		}
		g.gateway = gatewayAddr
	}

	g.initSharding()
	g.currentConnection = NewGatewayConnection(g)

	g.mu.Unlock()

	return g.currentConnection.Open()
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

// Close is a helper for Session.GatewayConnectionManager.Close()
func (s *Session) Close() error {
	return s.GatewayManager.Close()
}

func (g *GatewayConnectionManager) Close() (err error) {
	g.mu.Lock()
	if g.currentConnection != nil {
		err = g.currentConnection.Close()
		g.currentConnection = nil
	}
	g.mu.Unlock()
	return
}

func (g *GatewayConnectionManager) Reconnect() error {
	return g.Open()
}

type GatewayConnection struct {
	mu sync.Mutex

	// The parent manager
	manager *GatewayConnectionManager

	open   bool
	status GatewayStatus

	// Stores a mapping of guild id's to VoiceConnections
	voiceConnections map[string]*VoiceConnection

	// The underlying websocket connection.
	conn net.Conn

	// sequence tracks the current gateway api websocket sequence number
	sequence *int64

	// stores session ID of current Gateway connection
	sessionID string

	// This gets closed when the connection closes to signal all workers to stop
	stopWorkers chan interface{}

	reader      *GatewayReader
	heartbeater *wsHeartBeater
	writer      *wsWriter
}

func NewGatewayConnection(parent *GatewayConnectionManager) *GatewayConnection {
	return &GatewayConnection{
		manager:     parent,
		stopWorkers: make(chan interface{}),
	}
}

// Reconnect is a helper for Close() and Connect() and will attempt to resume if possible
func (g *GatewayConnection) Reconnect(forceReIdentify bool) error {
	return g.manager.Reconnect()
}

// Close closes the gateway connection
func (g *GatewayConnection) Close() error {
	g.mu.Lock()

	if g.open {
		close(g.stopWorkers)
	}

	g.open = false
	if g.conn == nil {
		g.mu.Unlock()
		return nil
	}

	err := g.conn.Close()
	g.mu.Unlock()

	return err
}

// Connect connects to the discord gateway and starts handling frames
func (g *GatewayConnection) Open() error {
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

	g.manager.session.log(LogInformational, "Connected to the gateway websocket")

	err = g.startWorkers()
	if err != nil {
		return err
	}

	g.mu.Unlock()

	return g.identify()
}

// startWorkers starts the background workers for reading, receiving and heartbeating
func (g *GatewayConnection) startWorkers() error {
	// Start the event reader
	reader, err := NewGatewayReader(g)
	if err != nil {
		return err
	}

	g.reader = reader
	go g.reader.read()

	// The writer
	writerWorker := &wsWriter{
		conn:     g.conn,
		session:  g.manager.session,
		closer:   g.stopWorkers,
		incoming: make(chan interface{}),
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
			g.manager.session.log(LogError, "No heartbeat ack received since sending last heartbeast, reconnecting...")
			g.Reconnect(false)
		},
	}

	return nil
}

// GatewayReader reads messages from the gateway and passes them to the connection for further handling
type GatewayReader struct {
	gw *GatewayConnection

	wsReader *wsutil.Reader

	// contains the raw message fragments until we have received them all
	messageBuffer *bytes.Buffer

	zlibReader  io.Reader
	jsonDecoder *json.Decoder
}

func NewGatewayReader(gw *GatewayConnection) (*GatewayReader, error) {
	reader := &GatewayReader{
		gw:            gw,
		wsReader:      wsutil.NewClientSideReader(gw.conn),
		messageBuffer: bytes.NewBuffer(make([]byte, 0, 0xffff)), // initial 65k buffer
	}

	return reader, nil
}

// Starts reading from the gateway
func (gr *GatewayReader) read() {

	// The buffer that is used to read into the bytes buffer
	// TODO: write it directly into the bytes buffer somehow to avoid a uneeded copy?
	intermediateBuffer := make([]byte, 0xffff)

	for {
		header, err := gr.wsReader.NextFrame()
		if err != nil {
			gr.gw.manager.session.log(LogError, "Error occurred reading next frame: %s", err.Error())
			gr.gw.Close()
			return
		}

		if !header.Fin {
			panic("welp should handle this")
		}

		for readAmount := int64(0); readAmount < header.Length; {

			n, err := gr.wsReader.Read(intermediateBuffer)
			gr.messageBuffer.Write(intermediateBuffer[:n])
			if err != nil {
				gr.gw.manager.session.log(LogError, "Error occured reading frame into buffer: %s", err.Error())
				gr.gw.Close()
				return
			}

			readAmount += int64(n)
		}

		gr.handleFrame(header)
	}
}

func (gr *GatewayReader) handleFrame(header ws.Header) {
	if header.OpCode == ws.OpClose {
		gr.handleCloseFrame(gr.messageBuffer.Bytes())
		gr.messageBuffer.Reset()
		return
	}

	if header.OpCode != ws.OpText && header.OpCode != ws.OpBinary {
		gr.messageBuffer.Reset()
		gr.gw.manager.session.log(LogError, "Don't know how to respond to websocket frame type: 0x%x", header.OpCode)
		return
	}

	if gr.messageBuffer.Len() < 4 {
		return
	}

	raw := gr.messageBuffer.Bytes()
	tail := raw[len(raw)-4:]

	if tail[0] != 0 || tail[1] != 0 || tail[2] != 0xff || tail[3] != 0xff {
		fmt.Println(tail)
		// Not the end of the packet
		return
	}

	gr.handleMessage()
	gr.messageBuffer.Reset()
}

// handleMessage is called when we have received a full message
// it decodes the message into an event and passes it up to the GatewayConnection
func (gr *GatewayReader) handleMessage() {

	if gr.zlibReader == nil {
		// We initialize the zlib reader here as opposed to in NewGatewayReader because
		// zlib.NewReader apperently needs the header straight away, or it will block forever
		zr, err := zlib.NewReader(gr.messageBuffer)
		if err != nil {
			gr.gw.manager.session.log(LogError, "Failed creating zlib reader: %s", err.Error())
			panic("well shit")
			return

		}

		gr.zlibReader = zr
		gr.jsonDecoder = json.NewDecoder(zr)
	}

	var event *Event
	err := gr.jsonDecoder.Decode(&event)
	if err != nil {
		gr.gw.manager.session.log(LogError, "Failed decoding incoming gateway event: %s", err)
		return
	}

	gr.gw.handleEvent(event)
}

func (gr *GatewayReader) handleCloseFrame(data []byte) {
	code := binary.BigEndian.Uint16(data)
	var msg string
	if len(data) > 2 {
		msg = string(data[2:])
	}
	gr.gw.manager.session.log(LogError, "Got close frame, code: %d, Msg: %q", code, msg)
	gr.gw.Close()
}

// handleEvent handles a event received from the GatewayReader
func (g *GatewayConnection) handleEvent(event *Event) {
	g.heartbeater.UpdateSequence(event.Sequence)

	var err error

	switch event.Operation {
	case GatewayOPDispatch:
		err = g.handleDispatch(event)
	case GatewayOPHeartbeat:
		g.heartbeater.SendBeat()
	case GatewayOPReconnect:
		err = g.Reconnect(false)
	case GatewayOPInvalidSession:
		g.sessionID = ""
		g.Reconnect(true)
	case GatewayOPHello:
		err = g.handleHello(event)
	case GatewayOPHeartbeatACK:
		g.heartbeater.ReceivedAck()
	default:
		g.manager.session.log(LogWarning, "Unknown operation (%d, %q): ", event.Operation, event.Type, string(event.RawData))
	}

	if err != nil {
		g.manager.session.log(LogError, "Error handling event (%d, %q): %s", event.Operation, event.Type, err.Error())
	}
}

func (g *GatewayConnection) handleHello(event *Event) error {
	var h helloData
	err := json.Unmarshal(event.RawData, &h)
	if err != nil {
		return err
	}

	g.manager.session.log(LogInformational, "Receivied hello, heartbeat_interval: %d, _trace: %v", h.HeartbeatInterval, h.Trace)

	go g.heartbeater.Run(time.Duration(h.HeartbeatInterval) * time.Millisecond)

	return nil
}

func (g *GatewayConnection) handleDispatch(e *Event) error {

	// Map event to registered event handlers and pass it along to any registered handlers.
	if eh, ok := registeredInterfaceProviders[e.Type]; ok {
		e.Struct = eh.New()

		// Attempt to unmarshal our event.
		if err := json.Unmarshal(e.RawData, e.Struct); err != nil {
			g.manager.session.log(LogError, "error unmarshalling %s event, %s", e.Type, err)
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
		g.manager.session.log(LogWarning, "unknown event: Op: %d, Seq: %d, Type: %s, Data: %s", e.Operation, e.Sequence, e.Type, string(e.RawData))
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

	g.writer.Queue(op)
	return nil
}

type outgoingEvent struct {
	Operation int         `json:"op"`
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
