package attach

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func parts(md5s ...string) []Part {
	out := make([]Part, 0, len(md5s))
	for i, m := range md5s {
		out = append(out, Part{
			MD5:         m,
			Filename:    "f.bin",
			ContentType: "application/octet-stream",
			Size:        int64(10 + i),
		})
	}
	return out
}

// The wire shape is a contract with logservice/src/query/hash.ts, and getting it
// wrong fails OPEN rather than loudly: routes/api.ts:93 does
// `Array.isArray(body) ? body : []`, so an object would be scanned against
// nothing and answered "allow".
func TestCheck_PostsABareArrayInTheLegacyShape(t *testing.T) {
	var gotBody []byte
	var gotKey, gotType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKey = r.Header.Get("X-API-Key")
		gotType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer srv.Close()

	s := &Scanner{URL: srv.URL, Timeout: time.Second, APIKey: "secret"}
	if _, err := s.Check(context.Background(), "AAAA.1", parts("d41d8cd98f00b204e9800998ecf8427e")); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if gotKey != "secret" {
		t.Errorf("X-API-Key = %q", gotKey)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q", gotType)
	}

	var list []map[string]any
	if err := json.Unmarshal(gotBody, &list); err != nil {
		t.Fatalf("body is not a JSON array: %v (%s)", err, gotBody)
	}
	if len(list) != 1 {
		t.Fatalf("sent %d descriptors, want 1", len(list))
	}
	for _, k := range []string{"md5", "contentType", "filename", "size", "txn_uuid"} {
		if _, ok := list[0][k]; !ok {
			t.Errorf("descriptor is missing %q: %v", k, list[0])
		}
	}
	if list[0]["txn_uuid"] != "AAAA.1" {
		t.Errorf("txn_uuid = %v", list[0]["txn_uuid"])
	}
}

func TestCheck_Verdicts(t *testing.T) {
	for _, want := range []Verdict{Allow, Block} {
		t.Run(string(want), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"action":"` + string(want) + `"}`))
			}))
			defer srv.Close()

			s := &Scanner{URL: srv.URL, Timeout: time.Second}
			got, err := s.Check(context.Background(), "A.1", parts("abc"))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != want {
				t.Errorf("verdict = %q, want %q", got, want)
			}
		})
	}
}

// Every one of these was "allow" in AttachChecker.js — three separate bypasses
// (:36 for transport and status, :35 for the missing action) sharing one
// symptom: a scanner outage silently stops scanning.
func TestCheck_FailuresAreErrorsNotAllow(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"404": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"action":"allow"}`))
		},
		"not JSON": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html>proxy error</html>`))
		},
		"200 with no action": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		},
		"200 with an unknown action": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"action":"maybe"}`))
		},
	}

	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()

			s := &Scanner{URL: srv.URL, Timeout: time.Second}
			got, err := s.Check(context.Background(), "A.1", parts("abc"))
			if err == nil {
				t.Fatalf("no error; verdict = %q", got)
			}
			if got != "" {
				t.Errorf("returned a verdict alongside an error: %q", got)
			}
		})
	}
}

func TestCheck_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	s := &Scanner{URL: srv.URL, Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := s.Check(context.Background(), "A.1", parts("abc")); err == nil {
		t.Fatal("a hung scanner did not produce an error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Check took %s; attach.timeout is not bounding it", d)
	}
}

// Not a round trip to save: with nothing to ask about, the answer is known.
func TestCheck_SkipsTheCallWhenThereIsNothingToScan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("posted a scan request with no digests")
	}))
	defer srv.Close()

	s := &Scanner{URL: srv.URL, Timeout: time.Second}
	for _, in := range [][]Part{nil, {{Filename: "empty.txt"}}} {
		got, err := s.Check(context.Background(), "A.1", in)
		if err != nil || got != Allow {
			t.Errorf("Check(%v) = %q, %v; want allow, nil", in, got, err)
		}
	}
}

// A proxy that answers with a megabyte of HTML should not become a megabyte of
// gateway memory per message.
func TestCheck_BoundsTheReplyItReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4*maxScanBody)))
	}))
	defer srv.Close()

	s := &Scanner{URL: srv.URL, Timeout: 2 * time.Second}
	if _, err := s.Check(context.Background(), "A.1", parts("abc")); err == nil {
		t.Fatal("an oversized non-JSON reply was accepted")
	}
}
