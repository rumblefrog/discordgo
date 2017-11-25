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
)

var (
	ErrAlreadyOpen = errors.New("Connection already open")
)

// Operation represents a gateway operation
// see https://discordapp.com/developers/docs/topics/gateway#gateway-opcodespayloads-gateway-opcodes
type Operation int

const (
	OPDispatch            Operation = 0  // (Receive)
	OPHeartbeat           Operation = 1  // (Send/Receive)
	OPIdentify            Operation = 2  // (Send)
	OPStatusUpdate        Operation = 3  // (Send)
	OPVoiceStateUpdate    Operation = 4  // (Send)
	OPVoiceServerPing     Operation = 5  // (Send)
	OPResume              Operation = 6  // (Send)
	OPReconnect           Operation = 7  // (Receive)
	OPRequestGuildMembers Operation = 8  // (Send)
	OPInvalidSession      Operation = 9  // (Receive)
	OPHello               Operation = 10 // (Receive)
	OPHeartbeatACK        Operation = 11 // (Receive)
)

type GatewayStatus int

const (
	GatewayStatusConnecting GatewayStatus = iota
	GatewayStatusIdentifying
	GatewayStatusReconnecting
	GatewayStatusNormal
)

type GatewayConnection struct {
	// used to make sure gateway websocket writes do not happen concurrently
	mu sync.Mutex

	open   bool
	status GatewayStatus

	// Stores a mapping of guild id's to VoiceConnections
	voiceConnections map[string]*VoiceConnection

	// The underlying websocket connection.
	conn net.Conn

	// When nil, the session is not listening.
	listening chan interface{}

	// sequence tracks the current gateway api websocket sequence number
	sequence *int64

	// stores sessions current Discord Gateway
	gateway string

	// stores session ID of current Gateway connection
	sessionID string

	shardCount int
	shardID    int

	session *Session

	writeChan chan interface{}

	stopWorkers chan interface{}

	reader *GatewayReader
}

func NewGatewayConnection(session *Session) *GatewayConnection {
	return &GatewayConnection{
		session:     session,
		stopWorkers: make(chan interface{}),
	}
}

// Open is a helper for Session.GatewayConnection.Open(s.Gateway())
func (s *Session) Open() error {
	gatewayAddr, err := s.Gateway()
	if err != nil {
		return err
	}

	return s.GatewayConnection.Open(gatewayAddr, s.ShardID, s.ShardCount)
}

// Close is a helper for Session.GatewayConnection.Close()
func (s *Session) Close() error {
	return s.GatewayConnection.Close()
}

// Reconnect is a helper for Close() and Connect() and will attempt to resume if possible
func (g *GatewayConnection) Reconnect(forceReIdentify bool) error {
	err := g.Close()
	if err != nil {
		return err
	}

	err = g.Connect()
	return err
}

// Open opens a new connection to the discord gateway
func (g *GatewayConnection) Open(gatewayAddr string, shardID, shardCount int) error {
	g.mu.Lock()

	g.initSharding(shardID, shardCount)
	g.gateway = gatewayAddr

	g.mu.Unlock()

	return g.Connect()
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

// initSharding sets the sharding details and verifies that they are valid
func (g *GatewayConnection) initSharding(shardID int, shardCount int) {
	g.shardCount = shardCount
	if g.shardCount < 1 {
		g.shardCount = 1
	}

	g.shardID = shardID
	if g.shardID >= shardCount || g.shardID < 0 {
		g.mu.Unlock()
		panic("Invalid shardID: ID:" + strconv.Itoa(g.shardID) + " Count:" + strconv.Itoa(g.shardCount))
	}

}

// Connect connects to the discord gateway and starts handling frames
func (g *GatewayConnection) Connect() error {
	g.mu.Lock()
	if g.open {
		g.mu.Unlock()
		return ErrAlreadyOpen
	}
	g.open = true

	conn, firstFrameReader, handshake, err := ws.Dial(context.TODO(), g.gateway+"?v="+APIVersion+"&encoding=json&compress=zlib-stream")
	if err != nil {
		g.mu.Unlock()
		if conn != nil {
			conn.Close()
		}
		return err
	}

	g.conn = conn

	fmt.Printf("connected! \n%#v\n\n%#v\n", firstFrameReader, handshake)

	reader, err := NewGatewayReader(g)
	if err != nil {
		g.Close()
		return err
	}
	g.reader = reader
	go g.reader.read()

	// We recreate and re-assign all these channels so that older workers from older connections
	// dosent have any effects on the new connection until they stop
	g.stopWorkers = make(chan interface{})
	g.writeChan = make(chan interface{})
	writerWorker := &wsWriter{
		conn:     conn,
		closer:   g.stopWorkers,
		GWConn:   g,
		incoming: g.writeChan,
	}
	go writerWorker.Run()

	g.mu.Unlock()

	return g.identify()
}

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

func (gr *GatewayReader) read() {

	intermediateBuffer := make([]byte, 0xffff)
	highestLength := int64(0)
	for {
		header, err := gr.wsReader.NextFrame()
		if err != nil {
			gr.gw.session.log(LogError, "Error occurred reading next frame: %s", err.Error())
			gr.gw.Close()
			return
		}

		if header.Length > highestLength {
			highestLength = header.Length

			fmt.Println("New highest legnth: ", float32(highestLength)/1000, "KB")
		}

		if !header.Fin {
			panic("welp should handle this")
		}

		for readAmount := int64(0); readAmount < header.Length; {

			n, err := gr.wsReader.Read(intermediateBuffer)
			gr.messageBuffer.Write(intermediateBuffer[:n])
			if err != nil {
				gr.gw.session.log(LogError, "Error occured reading frame into buffer: %s", err.Error())
				gr.gw.Close()
				return
			}

			readAmount += int64(n)
		}
		fmt.Println("Frame Body: hl: ", header.Length, ", ml:", gr.messageBuffer.Len())
		// fmt.Printf("%x\n\n", gr.messageBuffer.Bytes())
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
		panic("Oh shit boi: " + strconv.Itoa(int(header.OpCode)))
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

	gr.handlePacket()
	gr.messageBuffer.Reset()
}

func (gr *GatewayReader) handlePacket() {
	fmt.Println("End of packet: ", gr.messageBuffer.Len())

	if gr.zlibReader == nil {
		// We initialize the zlib reader here as opposed to in NewGatewayReader because
		// zlib.NewReader apperently needs the header straight away, or it will block forever
		zr, err := zlib.NewReader(gr.messageBuffer)
		if err != nil {
			gr.gw.session.log(LogError, "Failed creating zlib reader: %s", err.Error())
			panic("well shit")
			return

		}

		gr.zlibReader = zr
		gr.jsonDecoder = json.NewDecoder(zr)
	}

	var event *Event
	err := gr.jsonDecoder.Decode(&event)
	if err != nil {
		gr.gw.session.log(LogError, "Failed decoding incoming gateway event: %s", err)
		return
	}
}

func (gr *GatewayReader) handleCloseFrame(data []byte) {
	code := binary.BigEndian.Uint16(data)
	var msg string
	if len(data) > 2 {
		msg = string(data[2:])
	}
	gr.gw.session.log(LogError, "Got close frame, code: %d, Msg: %q", code, msg)
	gr.gw.Close()
}

func (g *GatewayConnection) handleEvent(event *Event) {
	var err error

	switch event.Operation {
	case OPDispatch:
		err = g.handleDispatch(event)
	case OPHeartbeat:
		// TODO: heartbeats
	case OPReconnect:
		err = g.Reconnect(false)
	case OPInvalidSession:
		g.sessionID = ""
		g.Reconnect(true)
	case OPHello:
		err = g.handleHello(event)
	case OPHeartbeatACK:
		// TODO: heartbeats
	default:
		g.session.log(LogWarning, "Unknown operation (%d, %q): ", event.Operation, event.Type, string(event.RawData))
	}
	if err != nil {
		g.session.log(LogError, "Error handling event (%d, %q): %s", event.Operation, event.Type, err.Error())
	}

	fmt.Println("Event: ", event.Operation, ", data: ", len(event.RawData), ", seq: ", event.Sequence, ", type: ", event.Type)
}

func (g *GatewayConnection) handleHello(event *Event) error {
	fmt.Println("Received hello: ", string(event.RawData))
	return nil
}

func (g *GatewayConnection) handleDispatch(e *Event) error {

	// Map event to registered event handlers and pass it along to any registered handlers.
	if eh, ok := registeredInterfaceProviders[e.Type]; ok {
		e.Struct = eh.New()

		// Attempt to unmarshal our event.
		if err := json.Unmarshal(e.RawData, e.Struct); err != nil {
			g.session.log(LogError, "error unmarshalling %s event, %s", e.Type, err)
		}

		// Send event to any registered event handlers for it's type.
		// Because the above doesn't cancel this, in case of an error
		// the struct could be partially populated or at default values.
		// However, most errors are due to a single field and I feel
		// it's better to pass along what we received than nothing at all.
		// TODO: Think about that decision :)
		// Either way, READY events must fire, even with errors.
		g.session.handleEvent(e.Type, e.Struct)
	} else {
		g.session.log(LogWarning, "unknown event: Op: %d, Seq: %d, Type: %s, Data: %s", e.Operation, e.Sequence, e.Type, string(e.RawData))
	}

	// For legacy reasons, we send the raw event also, this could be useful for handling unknown events.
	g.session.handleEvent(eventEventType, e)

	// fmt.Println("Received dispatch: ", string(event.RawData))
	return nil
}

func (g *GatewayConnection) write(data interface{}) {
	g.writeChan <- data
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
		Token:          g.session.Token,
		Properties:     properties,
		LargeThreshold: 250,
		// Compress:      g.session.Compress, // this is no longer needed since we use zlib-steam anyways
		Shard: nil,
	}

	if g.shardCount > 1 {
		data.Shard = &[2]int{g.shardID, g.shardCount}
	}

	op := outgoingEvent{
		Operation: 2,
		Data:      data,
	}

	g.write(op)
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

type wsWriter struct {
	GWConn *GatewayConnection

	conn     net.Conn
	closer   chan interface{}
	incoming chan interface{}

	writer *wsutil.Writer
}

func (w *wsWriter) Run() {
	w.writer = wsutil.NewWriter(w.conn, ws.StateClientSide, ws.OpText)

	for {
		select {
		case <-w.closer:
			return
		case msg := <-w.incoming:
			var err error
			switch t := msg.(type) {
			case []byte:
				err = w.writeRaw(t)
			default:
				err = w.writeJson(t)
			}

			if err != nil {
				w.GWConn.session.log(LogError, "Error writing to gateway: %s", err.Error())
				return
			}
		}
	}
}

func (w *wsWriter) writeJson(data interface{}) error {
	serialized, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return w.writeRaw(serialized)
}

func (w *wsWriter) writeRaw(data []byte) error {
	w.GWConn.session.log(LogInformational, "Writing %d bytes", len(data))
	fmt.Println(string(data))
	_, err := w.writer.WriteThrough(data)
	if err != nil {
		return err
	}

	return w.writer.Flush()
}
