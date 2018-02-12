package discordgo

import (
	"encoding/json"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type wsWriter struct {
	session *Session

	conn           net.Conn
	closer         chan interface{}
	incoming       chan interface{}
	sendCloseQueue chan ws.StatusCode

	writer *wsutil.Writer
}

func (w *wsWriter) Run() {
	w.writer = wsutil.NewWriter(w.conn, ws.StateClientSide, ws.OpText)

	for {
		select {
		case <-w.closer:
			select {
			// Ensure we send the close frame
			case code := <-w.sendCloseQueue:
				w.sendClose(code)
			default:
			}
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
				w.session.log(LogError, "Error writing to gateway: %s", err.Error())
				return
			}
		case code := <-w.sendCloseQueue:
			w.sendClose(code)
		}
	}
}

type requestGuildMembersData struct {
	GuildID string `json:"guild_id"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

type requestGuildMembersOp struct {
	Op   int                     `json:"op"`
	Data requestGuildMembersData `json:"d"`
}

func (w *wsWriter) writeJson(data interface{}) error {
	serialized, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return w.writeRaw(serialized)
}

func (w *wsWriter) writeRaw(data []byte) error {
	_, err := w.writer.WriteThrough(data)
	if err != nil {
		return err
	}

	return w.writer.Flush()
}

func (w *wsWriter) sendClose(code ws.StatusCode) error {

	d, err := ws.CompileFrame(ws.NewCloseFrame(code, ""))
	if err != nil {
		return err
	}

	_, err = w.conn.Write(d)
	return err
}

func (w *wsWriter) Queue(data interface{}) {
	select {
	case <-time.After(time.Second * 10):
	case <-w.closer:
	case w.incoming <- data:
	}
}

func (w *wsWriter) QueueClose(code ws.StatusCode) {
	select {
	case <-time.After(time.Second * 10):
	case <-w.closer:
	case w.sendCloseQueue <- code:
	}
}

// // onVoiceServerUpdate handles the Voice Server Update data websocket event.
// //
// // This is also fired if the Guild's voice region changes while connected
// // to a voice channel.  In that case, need to re-establish connection to
// // the new region endpoint.
// func (s *Session) onVoiceServerUpdate(st *VoiceServerUpdate) {

// 	s.log(LogInformational, "called")

// 	s.RLock()
// 	voice, exists := s.VoiceConnections[st.GuildID]
// 	s.RUnlock()

// 	// If no VoiceConnection exists, just skip this
// 	if !exists {
// 		return
// 	}

// 	// If currently connected to voice ws/udp, then disconnect.
// 	// Has no effect if not connected.
// 	voice.Close()

// 	// Store values for later use
// 	voice.Lock()
// 	voice.token = st.Token
// 	voice.endpoint = st.Endpoint
// 	voice.GuildID = st.GuildID
// 	voice.Unlock()

// 	// Open a connection to the voice server
// 	err := voice.open()
// 	if err != nil {
// 		s.log(LogError, "onVoiceServerUpdate voice.open, %s", err)
// >>>>>>> develop
// 	}
// }

type wsHeartBeater struct {
	sync.Mutex

	writer      *wsWriter
	sequence    *int64
	receivedAck bool
	stop        chan interface{}

	// Called when we received no Ack from last heartbeat
	onNoAck func()
}

func (wh *wsHeartBeater) ReceivedAck() {
	wh.Lock()
	wh.receivedAck = true
	wh.Unlock()
}

func (wh *wsHeartBeater) UpdateSequence(seq int64) {
	atomic.StoreInt64(wh.sequence, seq)
}

func (wh *wsHeartBeater) Run(interval time.Duration) {
	ticker := time.NewTicker(interval)

	for {
		select {
		case <-ticker.C:

			wh.Lock()
			hasReceivedAck := wh.receivedAck
			wh.receivedAck = false
			wh.Unlock()
			if !hasReceivedAck && wh.onNoAck != nil {
				wh.onNoAck()
			}

			wh.SendBeat()
		case <-wh.stop:
			return
		}
	}
}

func (wh *wsHeartBeater) SendBeat() {
	seq := atomic.LoadInt64(wh.sequence)

	wh.writer.Queue(&outgoingEvent{
		Operation: GatewayOPHeartbeat,
		Data:      seq,
	})
}
