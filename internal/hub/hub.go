// Package hub implements the bunk relay server: machine registry, pairing
// codes, peer ACLs and a blind E2E-encrypted relay over WebSocket.
package hub

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"bunk/internal/proto"
)

const codeTTL = 24 * time.Hour

// Hub is the relay server.
type Hub struct {
	mu       sync.Mutex
	store    *Store
	token    string
	clients  map[string]*client
	upgrader websocket.Upgrader
}

type client struct {
	id    string
	name  string
	conn  *websocket.Conn
	send  chan proto.Msg
	close chan struct{}
}

// New creates a hub. token is the shared secret clients must present;
// empty disables the check (local testing only).
func New(store *Store, token string) *Hub {
	return &Hub{
		store:   store,
		token:   token,
		clients: make(map[string]*client),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// Health reports liveness.
func (h *Hub) Health() (map[string]int, error) {
	return map[string]int{"machines": len(h.clients)}, nil
}

// ServeHTTP upgrades the connection and runs the client session.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	c := &client{
		conn:  conn,
		send:  make(chan proto.Msg, 64),
		close: make(chan struct{}),
	}
	go h.writer(c)
	h.readLoop(c)
	// readLoop returns on disconnect: unregister and broadcast.
	h.mu.Lock()
	if c.id != "" {
		delete(h.clients, c.id)
	}
	h.mu.Unlock()
	close(c.send)
	conn.Close()
	if c.id != "" {
		h.broadcastUpdate()
	}
}

// readLoop processes inbound messages until disconnect.
func (h *Hub) readLoop(c *client) {
	conn := c.conn
	conn.SetReadLimit(64 << 20) // 64 MiB frames (large docker layers)
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m proto.Msg
		if err := json.Unmarshal(raw, &m); err != nil {
			h.replyError(c, "bad message: "+err.Error())
			continue
		}
		h.handle(c, m)
	}
}

// writer serializes outbound messages.
func (h *Hub) writer(c *client) {
	for m := range c.send {
		if err := c.conn.WriteJSON(m); err != nil {
			return
		}
	}
}

func (h *Hub) reply(c *client, m proto.Msg) {
	select {
	case c.send <- m:
	case <-time.After(5 * time.Second):
	}
}

func (h *Hub) replyError(c *client, msg string) {
	h.reply(c, proto.Msg{Type: proto.TError, Message: msg})
}

// handle dispatches one inbound message.
func (h *Hub) handle(c *client, m proto.Msg) {
	switch m.Type {
	case proto.THello:
		h.onHello(c, m)
	case proto.TCreateCode:
		h.onCreateCode(c)
	case proto.TPair:
		h.onPair(c, m)
	case proto.TRevoke:
		h.onRevoke(c, m)
	case proto.TList:
		h.sendUpdate(c)
	case proto.TRelay:
		h.onRelay(c, m)
	case proto.TRelayClose:
		h.onRelayClose(c, m)
	default:
		h.replyError(c, "unknown message type: "+m.Type)
	}
}

func (h *Hub) onHello(c *client, m proto.Msg) {
	if m.MachineID == "" || m.PubKey == "" {
		h.replyError(c, "hello requires machine_id and pubkey")
		return
	}
	if h.token != "" && m.Token != h.token {
		h.replyError(c, "invalid hub token")
		return
	}
	if err := h.store.UpsertMachine(m.MachineID, m.Name, m.PubKey); err != nil {
		h.replyError(c, "store: "+err.Error())
		return
	}
	h.mu.Lock()
	if c.id != "" {
		delete(h.clients, c.id)
	}
	c.id = m.MachineID
	c.name = m.Name
	h.clients[c.id] = c
	h.mu.Unlock()
	h.reply(c, proto.Msg{Type: proto.TWelcome, MachineID: m.MachineID, Name: m.Name})
	h.broadcastUpdate()
}

func (h *Hub) onCreateCode(c *client) {
	if c.id == "" {
		h.replyError(c, "not registered")
		return
	}
	code, err := h.newCode(c.id)
	if err != nil {
		h.replyError(c, err.Error())
		return
	}
	h.reply(c, proto.Msg{Type: proto.TCodeCreated, Code: code})
}

func (h *Hub) onPair(c *client, m proto.Msg) {
	if c.id == "" {
		h.replyError(c, "not registered")
		return
	}
	owner, err := h.store.ConsumeCode(m.Code)
	if err != nil {
		h.replyError(c, err.Error())
		return
	}
	if owner == c.id {
		h.replyError(c, "cannot pair with yourself")
		return
	}
	// Ensure the guest machine is registered too.
	if err := h.store.UpsertMachine(c.id, m.Name, m.PubKey); err != nil {
		h.replyError(c, "store: "+err.Error())
		return
	}
	if err := h.store.AddACL(owner, c.id); err != nil {
		h.replyError(c, "store: "+err.Error())
		return
	}
	h.reply(c, proto.Msg{Type: proto.TPaired, Peer: &proto.MachineInfo{ID: owner}})
	h.broadcastUpdate()
}

func (h *Hub) onRevoke(c *client, m proto.Msg) {
	if c.id == "" || m.Target == "" {
		h.replyError(c, "revoke requires a target")
		return
	}
	if err := h.store.DeleteACL(c.id, m.Target); err != nil {
		h.replyError(c, "store: "+err.Error())
		return
	}
	h.reply(c, proto.Msg{Type: proto.TRevoked, Target: m.Target})
	h.broadcastUpdate()
}

func (h *Hub) onRelay(c *client, m proto.Msg) {
	if c.id == "" || m.To == "" || m.Channel == "" {
		return
	}
	ok, err := h.store.HasACL(c.id, m.To)
	if err != nil {
		return
	}
	if !ok {
		h.reply(c, proto.Msg{Type: proto.TError, Channel: m.Channel,
			Message: "no link to " + m.To + " (pair first, or ask the owner to pair)"})
		return
	}
	h.mu.Lock()
	target := h.clients[m.To]
	h.mu.Unlock()
	if target == nil {
		h.reply(c, proto.Msg{Type: proto.TError, Channel: m.Channel, Message: m.To + " is offline"})
		return
	}
	h.reply(target, proto.Msg{
		Type:    proto.TRelay,
		From:    c.id,
		Channel: m.Channel,
		Service: m.Service,
		Dial:    m.Dial,
		Data:    m.Data,
	})
}

func (h *Hub) onRelayClose(c *client, m proto.Msg) {
	if c.id == "" || m.To == "" || m.Channel == "" {
		return
	}
	h.mu.Lock()
	target := h.clients[m.To]
	h.mu.Unlock()
	if target == nil {
		return
	}
	h.reply(target, proto.Msg{
		Type:    proto.TRelayClose,
		From:    c.id,
		Channel: m.Channel,
	})
}

// sendUpdate pushes the machine list to one client.
func (h *Hub) sendUpdate(c *client) {
	h.reply(c, proto.Msg{Type: proto.TMachineUpdate, Machines: h.machineList()})
}

// broadcastUpdate pushes the machine list to all clients.
func (h *Hub) broadcastUpdate() {
	list := h.machineList()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		h.reply(c, proto.Msg{Type: proto.TMachineUpdate, Machines: list})
	}
}

func (h *Hub) machineList() []proto.MachineInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids, err := h.store.Machines()
	if err != nil {
		return nil
	}
	var out []proto.MachineInfo
	for _, id := range ids {
		pub, err := h.store.GetMachine(id)
		if err != nil {
			continue
		}
		c := h.clients[id]
		name := ""
		if c != nil {
			name = c.name
		}
		out = append(out, proto.MachineInfo{
			ID:     id,
			Name:   name,
			PubKey: pub,
			Online: c != nil,
		})
	}
	return out
}

func (h *Hub) newCode(owner string) (string, error) {
	code, err := randomCode()
	if err != nil {
		return "", err
	}
	if err := h.store.CreateCode(owner, code, codeTTL); err != nil {
		return "", err
	}
	return code, nil
}

func randomCode() (string, error) {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("rand: " + err.Error())
	}
	var sb strings.Builder
	for i, v := range b {
		if i == 3 {
			sb.WriteByte('-')
		}
		sb.WriteByte(chars[int(v)%len(chars)])
	}
	return sb.String(), nil
}
