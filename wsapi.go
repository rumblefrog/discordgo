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
				w.session.log(LogError, "Error writing to gateway: %s", err.Error())
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
	w.session.log(LogInformational, "Writing %d bytes", len(data))
	_, err := w.writer.WriteThrough(data)
	if err != nil {
		return err
	}

	return w.writer.Flush()
}

func (w *wsWriter) Queue(data interface{}) {
	select {
	case <-time.After(time.Second * 10):
	case w.incoming <- data:
	}
}

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
		Operation: int(GatewayOPHeartbeat),
		Data:      seq,
	})
}
