package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Verdict is what the blocklist said about a message.
type Verdict string

const (
	// Allow means no attachment's digest is on the blocklist.
	Allow Verdict = "allow"
	// Block means at least one is. logservice decides per message, not per
	// attachment: query/hash.ts decideActions blocks the message if ANY
	// attachment blocks.
	Block Verdict = "block"
)

// descriptor is one attachment as logservice expects it.
//
// The field names are a contract with logservice/src/query/hash.ts:7-13 and are
// byte-compatible with what AttachChecker.js:55-61 sent, so a mixed fleet writes
// the same HashLookups rows. Note the mixed casing — `contentType` is camel and
// `txn_uuid` is snake — which is logservice's, not ours to tidy.
type descriptor struct {
	MD5         string `json:"md5"`
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	TxnUUID     string `json:"txn_uuid"`
}

// Scanner posts attachment digests to logservice's blocklist endpoint.
//
// Deliberately without retries, unlike internal/events: this call sits inside an
// SMTP transaction with a client waiting on the DATA reply, so a retry only
// doubles how long that client is held before we answer with the same thing
// attach.fail was going to say anyway.
type Scanner struct {
	URL     string
	Timeout time.Duration
	// APIKey is sent as X-API-Key, matching AttachChecker.js:21-23. Empty is
	// valid: logservice accepts every request when its own API_KEY is unset.
	APIKey string
	// HTTP is optional; a zero Scanner builds its own client from Timeout.
	HTTP *http.Client
}

// maxScanBody bounds how much of a reply we read. The answer is a two-key JSON
// object; anything larger is a proxy error page and reading it in full only
// helps the thing serving it.
const maxScanBody = 64 << 10

// Check asks the blocklist about one message's attachments.
//
// An error is never a verdict. Every failure — transport, status, unparseable
// body, or a 200 whose action is neither "allow" nor "block" — comes back as an
// error so the caller applies attach.fail. AttachChecker.js collapsed all three
// into "allow" (:36, :77) and additionally produced an unhandled third state
// when a 200 carried no action at all (:35), which the plugin then read as a
// block.
func (s *Scanner) Check(ctx context.Context, txnUUID string, parts []Part) (Verdict, error) {
	list := make([]descriptor, 0, len(parts))
	for _, p := range parts {
		if p.MD5 == "" {
			continue
		}
		list = append(list, descriptor{
			MD5:         p.MD5,
			ContentType: p.ContentType,
			Filename:    p.Filename,
			Size:        p.Size,
			TxnUUID:     txnUUID,
		})
	}
	if len(list) == 0 {
		return Allow, nil
	}

	// A bare array, not an object wrapping one. logservice's handler does
	// `Array.isArray(body) ? body : []` (routes/api.ts:93), so any other shape
	// is scanned against nothing and answered "allow" — the exact silent bypass
	// this feature exists to close.
	body, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("attach: encode scan request: %w", err)
	}

	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("attach: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.APIKey != "" {
		req.Header.Set("X-API-Key", s.APIKey)
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("attach: scan %s: %w", s.URL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxScanBody))
	if err != nil {
		return "", fmt.Errorf("attach: read scan reply: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("attach: scan %s: http %d", s.URL, resp.StatusCode)
	}

	var out struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("attach: scan reply is not JSON: %w", err)
	}
	switch Verdict(out.Action) {
	case Allow:
		return Allow, nil
	case Block:
		return Block, nil
	default:
		return "", fmt.Errorf("attach: scan reply has no usable action (%q)", out.Action)
	}
}

func (s *Scanner) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	// The context already carries Timeout; this is the belt to its braces for a
	// server that accepts the connection and then never speaks.
	return &http.Client{Timeout: s.Timeout}
}
