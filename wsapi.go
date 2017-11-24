// Discordgo - Discord bindings for Go
// Available at https://github.com/bwmarrin/discordgo

// Copyright 2015-2016 Bruce Marriner <bruce@sqls.net>.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains low level functions for interacting with the Discord
// data websocket interface.

package discordgo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"net"
	"runtime"
	"strconv"
	"sync"
)

var (
	ErrAlreadyOpen = errors.New("Connection already open")
)

type Operation int

const (
	OPDispatch            Operation = 0
	OPHeartbeat           Operation = 1
	OPIdentify            Operation = 2
	OPStatusUpdate        Operation = 3
	OPVoiceStateUpdate    Operation = 4
	OPVoiceServerPing     Operation = 5
	OPResume              Operation = 6
	OPReconnect           Operation = 7
	OPRequestGuildMembers Operation = 8
	OPInvalidSession      Operation = 9
	OPHello               Operation = 10
	OPHeartbeatACK        Operation = 11
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

	conn, firstFrameReader, handshake, err := ws.Dial(context.TODO(), g.gateway+"?v="+APIVersion+"&encoding=json")
	if err != nil {
		g.mu.Unlock()
		if conn != nil {
			conn.Close()
		}
		return err
	}

	g.conn = conn

	fmt.Printf("connected! \n%#v\n\n%#v\n", firstFrameReader, handshake)

	go g.reader(wsutil.NewClientSideReader(conn))

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

func (g *GatewayConnection) reader(reader *wsutil.Reader) {
	buf := make([]byte, 0xffffff) // 16MB buffer, might be a bit large?
	for {
		header, err := reader.NextFrame()
		if err != nil {
			g.session.log(LogError, "Error occured reading next frame: %s", err.Error())
			g.Close()
			return
		}

		fmt.Printf("New frame! \n%#v\n\n", header)
		if int64(len(buf)) < header.Length {
			panic("Well fuck: " + strconv.FormatInt(header.Length, 10))
		}

		for readAmount := int64(0); readAmount < header.Length; {

			n, err := reader.Read(buf[readAmount:])
			if err != nil {
				g.session.log(LogError, "Error occured reading frame into buffer: %s", err.Error())
				g.Close()
				return
			}

			readAmount += int64(n)
		}

		fmt.Println("Frame Body: hl: ", header.Length)
		g.handleFrame(header, buf[:header.Length])
	}
}

func (g *GatewayConnection) handleFrame(header ws.Header, frame []byte) {
	if header.OpCode == ws.OpClose {
		g.handleCloseFrame(frame)
		return
	}

	fmt.Println("Got frame: ", string(frame))

}

func (g *GatewayConnection) handleCloseFrame(data []byte) {
	code := binary.BigEndian.Uint16(data)
	var msg string
	if len(data) > 2 {
		msg = string(data[2:])
	}
	g.session.log(LogError, "Got close frame, code: %d, Msg: %q", code, msg)
	g.Close()
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
		// Compress:      g.session.Compress,
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
