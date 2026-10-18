package spread

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsPingInterval = 10 * time.Second
	// wsReadTimeout drops a connection that has been silent for three ping intervals.
	wsReadTimeout = 3 * wsPingInterval
	wsMaxBackoff  = 30 * time.Second
)

// wsSub is one GateIO channel subscription.
type wsSub struct {
	Channel string
	Payload []string
}

func (s wsSub) key() string { return s.Channel + "|" + strings.Join(s.Payload, ",") }

// wsMessage is the GateIO v4 websocket envelope.
type wsMessage struct {
	Channel string `json:"channel"`
	Event   string `json:"event"`
	Error   *struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

type wsRequest struct {
	Time    int64    `json:"time"`
	Channel string   `json:"channel"`
	Event   string   `json:"event,omitempty"`
	Payload []string `json:"payload,omitempty"`
}

// wsConn is one GateIO websocket connection. It reconnects with backoff and restores its subscriptions each time.
type wsConn struct {
	name        string
	url         string
	pingChannel string
	handle      func(m wsMessage, received time.Time)
	onState     func(up bool)
	log         *slog.Logger

	up      atomic.Bool
	lastMsg atomic.Int64 // unix nanoseconds

	mu   sync.Mutex // guards conn, subs and writes
	conn *websocket.Conn
	subs map[string]wsSub
}

func newWSConn(name, url, pingChannel string, logger *slog.Logger, handle func(wsMessage, time.Time), onState func(bool)) *wsConn {
	return &wsConn{
		name:        name,
		url:         url,
		pingChannel: pingChannel,
		handle:      handle,
		onState:     onState,
		log:         logger,
		subs:        make(map[string]wsSub),
	}
}

// healthy reports whether the connection is up and has heard from the server recently.
func (c *wsConn) healthy() bool {
	return c.up.Load() && time.Since(time.Unix(0, c.lastMsg.Load())) < wsReadTimeout
}

// run keeps the connection alive until ctx is cancelled.
func (c *wsConn) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx)
		c.setUp(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		c.log.Warn("websocket disconnected, REST serves market data until it reconnects", "conn", c.name, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, wsMaxBackoff)
	}
}

func (c *wsConn) setUp(up bool) {
	if c.up.Swap(up) != up && c.onState != nil {
		c.onState(up)
	}
}

func (c *wsConn) session(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	c.lastMsg.Store(time.Now().UnixNano())

	c.mu.Lock()
	c.conn = conn
	subs := make([]wsSub, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	for _, s := range subs {
		if err := c.writeLocked("subscribe", s); err != nil {
			c.conn = nil
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()
	c.setUp(true)
	c.log.Info("websocket connected", "conn", c.name, "subscriptions", len(subs))

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				conn.Close()
				return
			case <-ticker.C:
				c.mu.Lock()
				err := conn.WriteJSON(wsRequest{Time: time.Now().Unix(), Channel: c.pingChannel})
				c.mu.Unlock()
				if err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
			return err
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		now := time.Now()
		c.lastMsg.Store(now.UnixNano())
		var m wsMessage
		if err := json.Unmarshal(data, &m); err != nil {
			c.log.Debug("unparseable websocket message", "conn", c.name, "err", err)
			continue
		}
		if m.Error != nil {
			c.log.Warn("websocket request rejected", "conn", c.name, "channel", m.Channel, "code", m.Error.Code, "message", m.Error.Message)
			continue
		}
		if m.Event == "update" || m.Event == "all" {
			c.handle(m, now)
		}
	}
}

// set replaces the desired subscriptions, subscribing and unsubscribing the difference on a live connection.
func (c *wsConn) set(desired []wsSub) {
	want := make(map[string]wsSub, len(desired))
	for _, s := range desired {
		want[s.key()] = s
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var removed, added []wsSub
	for k, s := range c.subs {
		if _, ok := want[k]; !ok {
			removed = append(removed, s)
		}
	}
	for k, s := range want {
		if _, ok := c.subs[k]; !ok {
			added = append(added, s)
		}
	}
	c.subs = want
	if c.conn == nil {
		return
	}
	// A failed write means the connection is going down; the next session subscribes to c.subs afresh.
	slices.SortFunc(added, func(a, b wsSub) int { return strings.Compare(a.key(), b.key()) })
	for _, s := range removed {
		if err := c.writeLocked("unsubscribe", s); err != nil {
			c.log.Debug("websocket unsubscribe", "conn", c.name, "channel", s.Channel, "err", err)
			return
		}
	}
	for _, s := range added {
		if err := c.writeLocked("subscribe", s); err != nil {
			c.log.Debug("websocket subscribe", "conn", c.name, "channel", s.Channel, "err", err)
			return
		}
	}
}

func (c *wsConn) writeLocked(event string, s wsSub) error {
	if err := c.conn.WriteJSON(wsRequest{Time: time.Now().Unix(), Channel: s.Channel, Event: event, Payload: s.Payload}); err != nil {
		return fmt.Errorf("%s %s: %w", event, s.Channel, err)
	}
	return nil
}
