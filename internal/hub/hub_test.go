package hub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"bunk/internal/e2e"
	"bunk/internal/proto"
)

type testClient struct {
	t    *testing.T
	c    *websocket.Conn
	kp   *e2e.KeyPair
	id   string
	msgs chan proto.Msg
}

func newHub(t *testing.T) (*Hub, string) {
	t.Helper()
	store, err := Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	h := New(store, "test-token")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return h, strings.TrimPrefix(ts.URL, "http://")
}

func dial(t *testing.T, addr, id, name string) *testClient {
	t.Helper()
	kp, err := e2e.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	tc := &testClient{t: t, c: c, kp: kp, id: id, msgs: make(chan proto.Msg, 256)}
	go tc.reader()
	tc.send(proto.Msg{
		Type:      proto.THello,
		MachineID: id,
		Name:      name,
		PubKey:    kp.PubB64,
		Token:     "test-token",
	})
	tc.recv(proto.TWelcome)
	return tc
}

// reader captures every inbound message so none are lost to ordering.
func (tc *testClient) reader() {
	for {
		_, raw, err := tc.c.ReadMessage()
		if err != nil {
			return
		}
		var m proto.Msg
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		select {
		case tc.msgs <- m:
		default: // queue full: drop; tests never push that hard
		}
	}
}

func (tc *testClient) send(m proto.Msg) {
	tc.t.Helper()
	if err := tc.c.WriteJSON(m); err != nil {
		tc.t.Fatal(err)
	}
}

// recv waits for the wanted message, skipping machine_update broadcasts.
func (tc *testClient) recv(want string) proto.Msg {
	tc.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-tc.msgs:
			if m.Type == proto.TMachineUpdate {
				continue
			}
			if m.Type != want {
				tc.t.Fatalf("got %q want %q (%+v)", m.Type, want, m)
			}
			return m
		case <-timeout:
			tc.t.Fatalf("timeout waiting for %q", want)
		}
	}
}

func TestPairingAndRelay(t *testing.T) {
	_, addr := newHub(t)
	host := dial(t, addr, "host", "brother")
	guest := dial(t, addr, "guest", "me")

	host.send(proto.Msg{Type: proto.TCreateCode})
	code := host.recv(proto.TCodeCreated).Code
	if len(code) < 6 {
		t.Fatalf("bad code %q", code)
	}

	guest.send(proto.Msg{Type: proto.TPair, Code: code, MachineID: "guest", Name: "me", PubKey: guest.kp.PubB64})
	paired := guest.recv(proto.TPaired)
	if paired.Peer == nil || paired.Peer.ID != "host" {
		t.Fatalf("bad pair response: %+v", paired)
	}

	// Relay a sealed frame host->guest via the hub.
	guestPub, _ := e2e.DecodePublicB64(guest.kp.PubB64)
	sealed, err := host.kp.Seal(guestPub, []byte("secret payload"))
	if err != nil {
		t.Fatal(err)
	}
	host.send(proto.Msg{
		Type:    proto.TRelay,
		To:      "guest",
		Channel: "ch1",
		Service: proto.ServiceDocker,
		Data:    base64.StdEncoding.EncodeToString(sealed),
	})
	m := guest.recv(proto.TRelay)
	if m.From != "host" || m.Channel != "ch1" || m.Service != proto.ServiceDocker {
		t.Fatalf("bad relay: %+v", m)
	}
	hostPub, _ := e2e.DecodePublicB64(host.kp.PubB64)
	raw, err := base64.StdEncoding.DecodeString(m.Data)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := guest.kp.Open(hostPub, raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != "secret payload" {
		t.Fatalf("payload mismatch: %q", plain)
	}

	host.send(proto.Msg{Type: proto.TRelayClose, To: "guest", Channel: "ch1"})
	guest.recv(proto.TRelayClose)
}

func TestRelayBlockedWithoutPairing(t *testing.T) {
	_, addr := newHub(t)
	a := dial(t, addr, "a", "a")
	dial(t, addr, "b", "b")

	a.send(proto.Msg{Type: proto.TRelay, To: "b", Channel: "x", Data: ""})
	m := a.recv(proto.TError)
	if !strings.Contains(m.Message, "pair") {
		t.Fatalf("expected pairing error, got %q", m.Message)
	}
}

func TestRevokeBlocksRelay(t *testing.T) {
	_, addr := newHub(t)
	host := dial(t, addr, "host", "brother")
	guest := dial(t, addr, "guest", "me")

	host.send(proto.Msg{Type: proto.TCreateCode})
	code := host.recv(proto.TCodeCreated).Code
	guest.send(proto.Msg{Type: proto.TPair, Code: code, MachineID: "guest", Name: "me", PubKey: guest.kp.PubB64})
	guest.recv(proto.TPaired)

	host.send(proto.Msg{Type: proto.TRevoke, Target: "guest"})
	host.recv(proto.TRevoked)

	host.send(proto.Msg{Type: proto.TRelay, To: "guest", Channel: "y", Data: ""})
	m := host.recv(proto.TError)
	if !strings.Contains(m.Message, "pair") {
		t.Fatalf("expected pairing error after revoke, got %q", m.Message)
	}
}

func TestInvalidTokenRejected(t *testing.T) {
	_, addr := newHub(t)
	kp, _ := e2e.GenerateKeyPair()
	c, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.WriteJSON(proto.Msg{Type: proto.THello, MachineID: "x", Name: "x", PubKey: kp.PubB64, Token: "wrong"})
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var m proto.Msg
	json.Unmarshal(raw, &m)
	if m.Type != proto.TError {
		t.Fatalf("expected error, got %+v", m)
	}
}

func TestPairingCodeOneTime(t *testing.T) {
	_, addr := newHub(t)
	host := dial(t, addr, "host", "brother")
	guest := dial(t, addr, "guest", "me")

	host.send(proto.Msg{Type: proto.TCreateCode})
	code := host.recv(proto.TCodeCreated).Code

	// Redeem successfully once.
	guest.send(proto.Msg{Type: proto.TPair, Code: code, MachineID: "guest", Name: "me", PubKey: guest.kp.PubB64})
	guest.recv(proto.TPaired)

	// A second guest cannot reuse the same code.
	guest2 := dial(t, addr, "guest2", "sneaky")
	guest2.send(proto.Msg{Type: proto.TPair, Code: code, MachineID: "guest2", Name: "sneaky", PubKey: guest2.kp.PubB64})
	m := guest2.recv(proto.TError)
	if !strings.Contains(m.Message, "invalid") {
		t.Fatalf("expected invalid-code error, got %q", m.Message)
	}
}
