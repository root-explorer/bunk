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
	Port int    `json:"port"`
	GPU  string `json:"gpu"`
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

	mu     sync.Mutex
	st     State
	peers  map[string]proto.MachineInfo
	online bool
	hubURL string

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
		err := d.connectOnce()
		if err != nil {
			log.Printf("hub connection: %v", err)
		}
		select {
		case <-d.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (d *Daemon) connectOnce() error {
	u, err := url.Parse(d.hubURL)
	if err != nil {
		return err
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
		return err
	}
	d.mu.Lock()
	d.conn = conn
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.conn = nil; d.mu.Unlock(); conn.Close() }()

	d.mu.Lock()
	d.online = false
	d.mu.Unlock()

	go d.hubWriter(conn)

	// hello: register this machine.
	d.sendMsg(proto.Msg{
		Type:      proto.THello,
		MachineID: d.st.MachineID,
		Name:      d.st.Name,
		PubKey:    d.st.Key.PubB64,
		Token:     d.Cfg.Token,
	})

	d.mu.Lock()
	d.online = true
	backoffReset := true
	d.mu.Unlock()
	_ = backoffReset

	// A control command may have queued while offline; nothing special.
	return d.hubReader(conn)
}

func (d *Daemon) sendMsg(m proto.Msg) {
	select {
	case d.send <- m:
	case <-d.stop:
	}
}

// hubWriter serializes outbound messages.
func (d *Daemon) hubWriter(conn *websocket.Conn) {
	for {
		select {
		case <-d.stop:
			return
		case m, ok := <-d.send:
			if !ok {
				return
			}
			if err := conn.WriteJSON(m); err != nil {
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
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
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
