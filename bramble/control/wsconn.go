package control

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
)

// wsConn adapts a gorilla *websocket.Conn to the control.Conn interface so the
// transport-agnostic Serve loop and request helpers work unchanged over a
// WebSocket. Each Msg is one WebSocket text frame (the WS layer already does
// message framing, so no newline delimiting is needed here).
type wsConn struct {
	ws  *websocket.Conn
	wmu sync.Mutex // gorilla requires one concurrent writer
}

// NewWSConn wraps a gorilla WebSocket connection as a control.Conn.
func NewWSConn(ws *websocket.Conn) Conn {
	return &wsConn{ws: ws}
}

func (c *wsConn) ReadMsg() (*Msg, error) {
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	var m Msg
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("control: decode ws msg: %w", err)
	}
	return &m, nil
}

func (c *wsConn) WriteMsg(m *Msg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *wsConn) Close() error { return c.ws.Close() }
