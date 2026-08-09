package daemon

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"bunk/internal/caps"
	"bunk/internal/e2e"
	"bunk/internal/proto"
)

// --- relay channels -------------------------------------------------------

// openChannel registers a new channel and sends the opening relay frame.
func (d *Daemon) openChannel(peerID, service, dial string) (*ch, error) {
	pub, err := d.peerPubKey(peerID)
	if err != nil {
		return nil, err
	}
	d.chMu.Lock()
	c := &ch{
		id:   newID(),
		peer: peerID,
		kind: kindFor(service),
		done: make(chan struct{}),
	}
	d.channels[c.id] = c
	d.chMu.Unlock()

	// Opening frame carries the service/dial header (empty data).
	if err := d.sendRelay(peerID, pub, c.id, service, dial, nil); err != nil {
		d.closeChannel(c.id, false)
		return nil, err
	}
	return c, nil
}

func kindFor(service string) string {
	switch service {
	case proto.ServiceCaps:
		return "caps"
	case proto.ServiceTCP:
		return "fwd"
	default:
		return "link"
	}
}

// peerPubKey returns the peer's E2E public key.
func (d *Daemon) peerPubKey(id string) (*[32]byte, error) {
	d.mu.Lock()
	p, ok := d.peers[id]
	d.mu.Unlock()
	if !ok || p.PubKey == "" {
		return nil, errors.New("peer not paired (run 'bunk pair' on both machines)")
	}
	return e2e.DecodePublicB64(p.PubKey)
}

// sendRelay seals and sends one frame to a peer over channel cid.
func (d *Daemon) sendRelay(peerID string, pub *[32]byte, cid, service, dial string, payload []byte) error {
	sealed, err := d.st.Key.Seal(pub, payload)
	if err != nil {
		return err
	}
	d.sendMsg(proto.Msg{
		Type:    proto.TRelay,
		To:      peerID,
		Channel: cid,
		Service: service,
		Dial:    dial,
		Data:    base64.StdEncoding.EncodeToString(sealed),
	})
	return nil
}

// closeChannel removes a channel and optionally notifies the peer.
func (d *Daemon) closeChannel(cid string, notifyPeer bool) {
	d.chMu.Lock()
	c, ok := d.channels[cid]
	if ok {
		c.closed = true
		delete(d.channels, cid)
	}
	d.chMu.Unlock()
	if !ok {
		return
	}
	if c.conn != nil {
		c.conn.Close()
	}
	if c.kind == "caps" {
		close(c.done)
	}
	if notifyPeer {
		d.sendMsg(proto.Msg{Type: proto.TRelayClose, To: c.peer, Channel: cid})
	}
}

// handleRelay processes an inbound frame. Channels are created lazily on
// the provider side by dialing the requested service.
func (d *Daemon) handleRelay(m proto.Msg) {
	d.chMu.Lock()
	c, exists := d.channels[m.Channel]
	d.chMu.Unlock()

	if exists {
		if c.kind == "caps" {
			d.appendCaps(c, m)
			return
		}
		if m.Data == "" {
			return
		}
		payload, err := d.openFrame(m.From, m.Data)
		if err != nil {
			return
		}
		d.chMu.Lock()
		cur, ok := d.channels[m.Channel]
		if ok {
			if cur.conn != nil {
				cur.conn.Write(payload)
			} else {
				// not attached yet: buffer until pipeLocal attaches
				cur.caps = append(cur.caps, payload...)
			}
		}
		d.chMu.Unlock()
		return
	}

	// New channel from a peer: dial the requested service.
	pub, err := d.peerPubKey(m.From)
	if err != nil {
		d.sendMsg(proto.Msg{Type: proto.TRelayClose, To: m.From, Channel: m.Channel})
		return
	}
	payload, err := d.openFrame(m.From, m.Data)
	if err != nil {
		return // unauthenticated frame: drop
	}

	switch m.Service {
	case proto.ServiceCaps:
		d.chMu.Lock()
		cc := &ch{id: m.Channel, peer: m.From, kind: "caps", done: make(chan struct{})}
		d.channels[m.Channel] = cc
		d.chMu.Unlock()
		capsJSON := d.capsJSON()
		d.sendRelay(m.From, pub, m.Channel, "", "", capsJSON)
		d.sendMsg(proto.Msg{Type: proto.TRelayClose, To: m.From, Channel: m.Channel})
		d.closeChannel(m.Channel, false)
		return

	case proto.ServiceTCP:
		if !d.Cfg.ServeDocker && m.Dial == "" {
			return
		}
		conn, err := net.DialTimeout("tcp", m.Dial, 5*time.Second)
		if err != nil {
			d.sendMsg(proto.Msg{Type: proto.TRelayClose, To: m.From, Channel: m.Channel})
			return
		}
		d.registerDialed(m.Channel, m.From, conn)
		if len(payload) > 0 {
			conn.Write(payload)
		}
		go d.pumpConnToPeer(m.Channel)

	case proto.ServiceDocker:
		if !d.Cfg.ServeDocker {
			log.Printf("denying docker access from %s (serve_docker is off)", m.From)
			d.sendMsg(proto.Msg{Type: proto.TRelayClose, To: m.From, Channel: m.Channel})
			return
		}
		conn, err := d.dialDocker()
		if err != nil {
			log.Printf("docker dial: %v", err)
			d.sendMsg(proto.Msg{Type: proto.TRelayClose, To: m.From, Channel: m.Channel})
			return
		}
		d.registerDialed(m.Channel, m.From, conn)
		if len(payload) > 0 {
			conn.Write(payload)
		}
		go d.pumpConnToPeer(m.Channel)
	}
}

func (d *Daemon) registerDialed(cid, peer string, conn net.Conn) {
	d.chMu.Lock()
	d.channels[cid] = &ch{id: cid, peer: peer, kind: "link", conn: conn}
	d.chMu.Unlock()
}

// handleRelayClose tears down a channel from the peer's side.
func (d *Daemon) handleRelayClose(m proto.Msg) {
	d.closeChannel(m.Channel, false)
}

// pumpConnToPeer reads from a dialed conn and relays sealed frames back.
func (d *Daemon) pumpConnToPeer(cid string) {
	d.chMu.Lock()
	c, ok := d.channels[cid]
	d.chMu.Unlock()
	if !ok {
		return
	}
	pub, err := d.peerPubKey(c.peer)
	if err != nil {
		d.closeChannel(cid, true)
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 {
			if serr := d.sendRelay(c.peer, pub, cid, "", "", buf[:n]); serr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	d.closeChannel(cid, true)
}

// openFrame decrypts and authenticates an inbound sealed frame.
func (d *Daemon) openFrame(fromID, dataB64 string) ([]byte, error) {
	d.mu.Lock()
	p, ok := d.peers[fromID]
	d.mu.Unlock()
	if !ok || p.PubKey == "" {
		return nil, errors.New("unknown peer")
	}
	senderPub, err := e2e.DecodePublicB64(p.PubKey)
	if err != nil {
		return nil, err
	}
	sealed, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, err
	}
	return d.st.Key.Open(senderPub, sealed)
}

func (d *Daemon) appendCaps(c *ch, m proto.Msg) {
	payload, err := d.openFrame(m.From, m.Data)
	if err != nil {
		return
	}
	c.caps = append(c.caps, payload...)
}

// --- caps service (provider side) -----------------------------------------

func (d *Daemon) capsJSON() []byte {
	c := caps.Detect()
	out, _ := json.Marshal(c)
	return out
}

// --- docker dialing (provider side) ---------------------------------------

func (d *Daemon) dialDocker() (net.Conn, error) {
	host := os.Getenv("DOCKER_HOST")
	switch {
	case host == "":
		return net.Dial("unix", "/var/run/docker.sock")
	case len(host) > 7 && host[:7] == "unix://":
		return net.Dial("unix", host[7:])
	case len(host) > 6 && host[:6] == "tcp://":
		return net.Dial("tcp", host[6:])
	default:
		return net.Dial("tcp", host)
	}
}
