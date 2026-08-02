package central

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsConsole is a stand-in for the console's GET /agent/ws. It verifies the
// upgrade signature exactly as verifyLikeConsole does for the other routes,
// which is what stops the two sides drifting apart.
type wsConsole struct {
	t *testing.T

	upgrades chan struct{}

	// mu guards send, which the test replaces between connections while a
	// handler goroutine may still hold the previous one.
	mu   sync.Mutex
	send chan string

	// unauthorized makes the handler reject the upgrade, standing in for a
	// gateway the console has forgotten.
	unauthorized bool
}

func newWSConsole(t *testing.T) *wsConsole {
	return &wsConsole{t: t, upgrades: make(chan struct{}, 8), send: make(chan string, 8)}
}

// channel hands a handler the send channel current at the moment it connected.
func (s *wsConsole) channel() chan string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send
}

// hangUp closes the current connection's channel and installs a fresh one, so
// the next connection gets a live channel.
func (s *wsConsole) hangUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.send)
	s.send = make(chan string, 8)
}

func (s *wsConsole) push(msg string) {
	s.channel() <- msg
}

func (s *wsConsole) handler(pub []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/ws" {
			http.NotFound(w, r)
			return
		}
		if s.unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The whole point of routing this through the normal signing path: the
		// upgrade is authenticated like every other request, so this route adds
		// no new authentication mechanism.
		verifyLikeConsole(s.t, pub, r)
		if r.Header.Get("X-GW-Id") == "" {
			s.t.Error("the upgrade must carry X-GW-Id")
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			s.t.Errorf("accept: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()

		send := s.channel()
		s.upgrades <- struct{}{}

		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-send:
				if !ok {
					_ = conn.Close(websocket.StatusNormalClosure, "")
					return
				}
				if err := conn.Write(r.Context(), websocket.MessageText, []byte(msg)); err != nil {
					return
				}
			}
		}
	})
}

func TestWatch_SignedUpgradeAndNotification(t *testing.T) {
	pub, key := testKey(t)

	console := newWSConsole(t)
	srv := httptest.NewServer(console.handler(pub))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, ID: "gw-1", Key: key}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	changed := make(chan struct{}, 8)
	go func() { _ = c.Watch(ctx, func() { changed <- struct{}{} }) }()

	select {
	case <-console.upgrades:
	case <-ctx.Done():
		t.Fatal("the socket never connected")
	}

	console.push(`{"event":"changed"}`)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("the notification never reached the caller")
	}
}

// A dropped socket must come back on its own — a console restart is routine.
func TestWatch_Reconnects(t *testing.T) {
	pub, key := testKey(t)

	console := newWSConsole(t)
	srv := httptest.NewServer(console.handler(pub))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, ID: "gw-1", Key: key}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	go func() { _ = c.Watch(ctx, func() {}) }()

	select {
	case <-console.upgrades:
	case <-ctx.Done():
		t.Fatal("the socket never connected")
	}

	// Close the connection from the console's side.
	console.hangUp()

	select {
	case <-console.upgrades:
	case <-ctx.Done():
		t.Fatal("the socket never reconnected after a clean close")
	}
}

// Watch must return only when its context ends, no matter what the console
// does. Anything else would make a management-plane failure stop the gateway.
func TestWatch_ReturnsOnlyOnContextCancel(t *testing.T) {
	pub, key := testKey(t)

	console := newWSConsole(t)
	console.unauthorized = true
	srv := httptest.NewServer(console.handler(pub))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, ID: "gw-1", Key: key}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, func() {}) }()

	select {
	case err := <-done:
		t.Fatalf("Watch returned while the context was live: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("Watch returned %v, want the context error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after cancellation")
	}
}

func TestWatch_RefusesWithoutIdentity(t *testing.T) {
	_, key := testKey(t)

	// Not registered yet: there is no gateway id to present, so there is
	// nothing to connect as.
	c := &Client{BaseURL: "https://console.invalid", Key: key}
	if err := c.watchOnce(context.Background(), func() {}); err == nil {
		t.Fatal("expected an error without a gateway id")
	}
}

func TestWSURL(t *testing.T) {
	cases := map[string]string{
		"https://console.example:4000":    "wss://console.example:4000/agent/ws",
		"http://console.example:3000":     "ws://console.example:3000/agent/ws",
		"https://console.example/mailgw":  "wss://console.example/mailgw/agent/ws",
		"https://console.example/mailgw/": "wss://console.example/mailgw/agent/ws",
	}
	for base, want := range cases {
		got, err := (&Client{BaseURL: base}).wsURL()
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", base, got, want)
		}
	}
}
