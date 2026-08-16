// Package daemon implements the bunk client agent: it dials out to the
// hub, maintains the E2E relay, exposes the local docker daemon to
// authorized peers, and hosts local proxy sockets for linked machines.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"bunk/internal/caps"
	"bunk/internal/e2e"
	"bunk/internal/proto"
)

// Defaults matched to the plan: respect-the-host limits.
const (
	DefaultCPUs     = 6
	DefaultMemoryGB = 12
	DefaultPids     = 256

	// welcomeTimeout bounds how long we wait for the hub to ack our hello
	// before failing the connection and retrying.
	welcomeTimeout = 15 * time.Second
)

// Limits are injected into docker run unless the user set them.
type Limits struct {
	CPUs     int `yaml:"cpus"`
	MemoryGB int `yaml:"memory_gb"`
	Pids     int `yaml:"pids"`
}

// IdleGateConfig controls the optional pause-when-active add-on.
type IdleGateConfig struct {
	Enabled     bool `yaml:"enabled"`
	IdleSeconds int  `yaml:"idle_seconds"`
	ThresholdMs int  `yaml:"threshold_ms"`
}

// Config is ~/.bunk/config.yaml.
type Config struct {
	Hub         string         `yaml:"hub"`
	Token       string         `yaml:"token"`
	ServeDocker bool           `yaml:"serve_docker"`
	GPU         string         `yaml:"gpu"` // auto | off
	Limits      Limits         `yaml:"limits"`
	IdleGate    IdleGateConfig `yaml:"idle_gate"`
}

// DefaultConfig returns the plan's defaults.
func DefaultConfig() Config {
	return Config{
		ServeDocker: true,
		GPU:         "auto",
		Limits:      Limits{CPUs: DefaultCPUs, MemoryGB: DefaultMemoryGB, Pids: DefaultPids},
	}
}

// LinkInfo is a live docker proxy listener for a peer machine.
type LinkInfo struct {
	Port  int    `json:"port"`
	GPU   string `json:"gpu"`
	Cores int    `json:"cores,omitempty"`
	RAMGB int    `json:"ram_gb,omitempty"`
}

// State is ~/.bunk/state.json, written by the daemon, read by the CLI.
type State struct {
	MachineID   string               `json:"machine_id"`
	Key         *e2e.KeyPair         `json:"key"`
	Name        string               `json:"name"`
	ControlPort int                  `json:"control_port"`
	Active      string               `json:"active"`
	Links       map[string]*LinkInfo `json:"links"`
	Forwards    []*proto.ForwardInfo `json:"forwards"`
}

// ch is one relay channel (a bidirectional byte pipe to a peer).
type ch struct {
	id     string
	peer   string
	kind   string // link | fwd | caps
	conn   net.Conn
	caps   []byte
	done   chan struct{}
	closed bool
}

// Daemon is the client agent.
type Daemon struct {
	Home string
	Cfg  Config

	mu       sync.Mutex
	st       State
	peers    map[string]proto.MachineInfo
	online   bool
	welcomed bool
	hubURL   string

	writerMu   sync.Mutex
	writerStop chan struct{}

	conn *websocket.Conn
	send chan proto.Msg

	chMu     sync.Mutex
	channels map[string]*ch
	linkLns  map[string]net.Listener
	fwdLns   map[int]net.Listener

	pendingCode    chan string
	pendingCodeErr chan string
	pendingPair    chan error

	ctrlLn net.Listener
	stop   chan struct{}
	doneCh chan struct{}
}

// New creates a daemon rooted at home (defaults to ~/.bunk).
func New(home string) (*Daemon, error) {
	if home == "" {
		hd, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(hd, ".bunk")
	}
	d := &Daemon{
		Home:     home,
		peers:    map[string]proto.MachineInfo{},
		send:     make(chan proto.Msg, 128),
		channels: map[string]*ch{},
		linkLns:  map[string]net.Listener{},
		fwdLns:   map[int]net.Listener{},
		stop:     make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	if err := d.loadConfig(); err != nil {
		return nil, err
	}
	if err := d.loadState(); err != nil {
		return nil, err
	}
	d.hubURL = d.Cfg.Hub
	if d.hubURL == "" {
		if h := os.Getenv("BUNK_HUB"); h != "" {
			d.hubURL = h
		}
	}
	if d.Cfg.Token == "" {
		d.Cfg.Token = os.Getenv("BUNK_HUB_TOKEN")
	}
	return d, nil
}

func (d *Daemon) loadConfig() error {
	path := filepath.Join(d.Home, "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			d.Cfg = DefaultConfig()
			return nil
		}
		return err
	}
	c := DefaultConfig()
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	d.Cfg = c
	return nil
}

func (d *Daemon) loadState() error {
	path := filepath.Join(d.Home, "state.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &d.st); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if d.st.Key == nil {
		kp, err := e2e.GenerateKeyPair()
		if err != nil {
			return err
		}
		d.st.Key = kp
	} else {
		// Public/Private are json:"-", so they are nil after unmarshal.
		// Re-derive them or the first Seal/Open will panic.
		kp, err := e2e.LoadKey(d.st.Key.PubB64, d.st.Key.PrivB64)
		if err != nil {
			return fmt.Errorf("state key: %w", err)
		}
		d.st.Key = kp
	}
	if d.st.MachineID == "" {
		d.st.MachineID = newID()
	}
	if d.st.Name == "" {
		d.st.Name = caps.Detect().Hostname
	}
	if d.st.Links == nil {
		d.st.Links = map[string]*LinkInfo{}
	}
	return d.saveState()
}

func (d *Daemon) saveState() error {
	path := filepath.Join(d.Home, "state.json")
	raw, err := json.MarshalIndent(&d.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Run starts the control server and the hub connect loop, blocking until
// Stop is called.
func (d *Daemon) Run() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	d.ctrlLn = ln
	d.mu.Lock()
	d.st.ControlPort = ln.Addr().(*net.TCPAddr).Port
	d.mu.Unlock()
	d.saveState()
	go d.controlLoop()

	d.restoreListeners()
	go d.watchAutoForwards()

	if d.Cfg.IdleGate.Enabled {
		go d.runIdleGate()
	}

	go d.connectLoop()
	<-d.stop
	close(d.doneCh)
	return nil
}

// Stop terminates the daemon.
func (d *Daemon) Stop() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	if d.conn != nil {
		d.conn.Close()
	}
}

// connectLoop keeps the hub connection alive with backoff.
func (d *Daemon) connectLoop() {
	backoff := time.Second
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		if d.hubURL == "" {
			log.Printf("no hub configured (set hub: in %s/config.yaml or BUNK_HUB)", d.Home)
			time.Sleep(5 * time.Second)
			continue
		}
		welcomed, err := d.connectOnce()
		if err != nil {
			log.Printf("hub connection: %v", err)
		}
		if welcomed {
			backoff = time.Second // we were connected: retry fast after drops
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
		select {
		case <-d.stop:
			return
		case <-time.After(backoff):
		}
	}
}

// connectOnce dials the hub, registers this machine, and blocks serving the
// connection. Returns (welcomed, err): welcomed is true when the hub
// acknowledged our hello (so the caller can reset its backoff).
func (d *Daemon) connectOnce() (bool, error) {
	u, err := url.Parse(d.hubURL)
	if err != nil {
		return false, err
	}
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else if u.Scheme == "https" {
		u.Scheme = "wss"
	}
	if !strings.HasSuffix(u.Path, "/ws") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return false, err
	}
	d.mu.Lock()
	d.conn = conn
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.conn = nil; d.mu.Unlock(); conn.Close() }()

	d.mu.Lock()
	d.online = false
	d.welcomed = false
	d.mu.Unlock()

	// Stop the previous connection's writer before starting this one: the
	// send channel is shared across connections, and a zombie writer would
	// steal messages (the hello!) and write them into a dead socket.
	d.writerMu.Lock()
	if d.writerStop != nil {
		close(d.writerStop)
	}
	d.writerStop = make(chan struct{})
	stop := d.writerStop
	d.writerMu.Unlock()
	go d.hubWriter(conn, stop)

	// hello: register this machine. Written directly on this connection so
	// it can never be stolen by a previous writer.
	if err := conn.WriteJSON(proto.Msg{
		Type:      proto.THello,
		MachineID: d.st.MachineID,
		Name:      d.st.Name,
		PubKey:    d.st.Key.PubB64,
		Token:     d.Cfg.Token,
	}); err != nil {
		return false, err
	}

	// Wait up to welcomeTimeout for the hub's welcome; if the hub stalls
	// (e.g. wedged store), fail the connection so the loop retries instead
	// of blocking forever.
	if err := conn.SetReadDeadline(time.Now().Add(welcomeTimeout)); err != nil {
		return false, err
	}
	err = d.hubReader(conn)
	d.mu.Lock()
	welcomed := d.welcomed
	d.mu.Unlock()
	return welcomed, err
}

func (d *Daemon) sendMsg(m proto.Msg) {
	select {
	case d.send <- m:
	case <-d.stop:
	}
}

// hubWriter serializes outbound messages and keeps the connection
// alive with periodic pings (read deadlines on both ends rely on them).
func (d *Daemon) hubWriter(conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-stop:
			return
		case m, ok := <-d.send:
			if !ok {
				return
			}
			if err := conn.WriteJSON(m); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}

// hubReader processes inbound hub messages until disconnect.
func (d *Daemon) hubReader(conn *websocket.Conn) error {
	conn.SetReadLimit(64 << 20)
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	for {
		d.mu.Lock()
		welcomed := d.welcomed
		d.mu.Unlock()
		if welcomed {
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		} else {
			// Still waiting for the hub's welcome: keep the short
			// deadline so a stalled hub fails fast and we retry.
			conn.SetReadDeadline(time.Now().Add(welcomeTimeout))
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var m proto.Msg
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		d.handleHubMsg(m)
	}
}

func (d *Daemon) handleHubMsg(m proto.Msg) {
	switch m.Type {
	case proto.TMachineUpdate:
		d.mu.Lock()
		d.peers = map[string]proto.MachineInfo{}
		for _, p := range m.Machines {
			d.peers[p.ID] = p
		}
		d.mu.Unlock()
	case proto.TWelcome:
		log.Printf("connected to hub as %q (%s)", m.Name, m.MachineID)
		d.mu.Lock()
		d.online = true
		d.welcomed = true
		d.mu.Unlock()
		if d.conn != nil {
			// Welcome received: lift the welcome deadline; keepalive
			// deadlines take over from here.
			d.conn.SetReadDeadline(time.Time{})
		}
	case proto.TPaired:
		log.Printf("paired with %s", m.Peer.ID)
	case proto.TCodeCreated:
		log.Printf("pairing code: %s", m.Code)
	case proto.TRevoked:
		log.Printf("revoked link to %s", m.Target)
	case proto.TError:
		log.Printf("hub error: %s", m.Message)
		if m.Channel != "" {
			// A relay was rejected (no link / peer offline): fail the
			// pending local connection fast instead of hanging.
			d.closeChannel(m.Channel, false)
		}
	case proto.TRelay:
		d.handleRelay(m)
	case proto.TRelayClose:
		d.handleRelayClose(m)
	}
	d.handleHubMsgExtras(m)
}

// newID returns a random 16-byte hex id.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
