package central

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Watch holds a WebSocket open to the console and calls onChange whenever it
// says something about this gateway has changed — a deploy, a rollback, an
// approval.
//
// # What this is, and what it is not
//
// It is an optimisation on the poll loop, nothing more. Frames carry no state:
// the gateway is told "go and look", and it looks through the same
// /agent/status it would have polled anyway. A duplicated, delayed or spurious
// notification therefore costs one cheap request and can never deliver wrong
// state, and a socket that never connects at all costs only latency.
//
// That is deliberate. Making the mail path depend on a long-lived connection to
// a management console would be exactly the wrong trade for a mail relay.
//
// Watch blocks until ctx is cancelled, reconnecting with backoff after any
// failure. It returns only ctx.Err().
func (c *Client) Watch(ctx context.Context, onChange func()) error {
	backoff := wsMinBackoff

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := c.watchOnce(ctx, onChange)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			// A clean close from the console (a restart, a deploy of the
			// console itself). Reconnect promptly.
			backoff = wsMinBackoff
		} else {
			c.log().Debug("gateway notification socket dropped; falling back to polling until it reconnects",
				"err", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, wsMaxBackoff)
	}
}

const (
	wsMinBackoff = 2 * time.Second
	wsMaxBackoff = 2 * time.Minute
	// wsReadLimit caps a single frame. The console only ever sends a tiny JSON
	// object, so anything larger is a misconfigured endpoint rather than a
	// message worth buffering.
	wsReadLimit = 4096
)

// watchOnce dials, reads until the connection ends, and returns why.
func (c *Client) watchOnce(ctx context.Context, onChange func()) error {
	if len(c.Key) == 0 {
		return errors.New("central: watch: no signing key")
	}
	if c.ID == "" {
		return errors.New("central: watch: not registered yet")
	}

	target, err := c.wsURL()
	if err != nil {
		return err
	}

	// The upgrade request is signed exactly like every other call — a GET with
	// no body, so the digest is sha256(""). The console runs the same
	// authenticateGateway hook on it, which is why this route needs no
	// authentication mechanism of its own.
	signed, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("central: watch: %w", err)
	}
	header := c.sign(http.MethodGet, signed.RequestURI(), nil)
	// Set by the WebSocket handshake itself; leaving ours would be rejected.
	header.Del("Content-Type")

	conn, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPClient: c.wsHTTPClient(),
		HTTPHeader: header,
	})
	if err != nil {
		return fmt.Errorf("central: watch: dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()

	conn.SetReadLimit(wsReadLimit)
	c.log().Info("connected to Central Management for change notifications")

	for {
		// No read deadline: the console is silent on an idle fleet by design,
		// and a gateway that reconnects every 30 seconds to prove the socket is
		// alive would cost more than it saves. A dead connection is caught by
		// TCP keepalive, and the poll loop covers the gap regardless.
		if _, _, err := conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.StatusNormalClosure {
				return nil
			}
			return err
		}
		// The payload is deliberately ignored. Whatever the console said, the
		// answer is the same: ask it what the current state is.
		onChange()
	}
}

// wsURL turns the console's base URL into the notification endpoint, honouring
// any path prefix the way every other call does.
func (c *Client) wsURL() (string, error) {
	base, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil {
		return "", fmt.Errorf("central: watch: invalid base URL %q: %w", c.BaseURL, err)
	}
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + "/agent/ws"

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	return u.String(), nil
}

// wsHTTPClient is the transport for the upgrade handshake.
//
// It must NOT negotiate HTTP/2. The console's WebSocket support is @fastify/ws,
// which speaks the HTTP/1.1 Upgrade handshake and not RFC 8441 extended CONNECT
// over h2 — and the console serves h2 with allowHTTP1 enabled precisely so this
// works. NewHTTPClient sets ForceAttemptHTTP2, so this clones it and turns that
// off rather than sharing it.
//
// The timeout is also dropped: http.Client.Timeout bounds the whole exchange,
// which for a connection meant to live for the life of the process would close
// it on a fixed schedule.
func (c *Client) wsHTTPClient() *http.Client {
	src := c.httpClient()

	tr, ok := src.Transport.(*http.Transport)
	if !ok {
		return &http.Client{Transport: src.Transport}
	}
	clone := tr.Clone()
	clone.ForceAttemptHTTP2 = false
	// A cloned transport keeps h2 registered in TLSNextProto unless it is
	// cleared, which would let ALPN pick h2 anyway.
	clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}

	return &http.Client{Transport: clone}
}
