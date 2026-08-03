package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/central"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// The whole milestone in one test: a gateway with nothing, a console that
// deploys to it, and mail service that starts as a result.
//
// The pull-loop tests above use a fake applier to isolate the bookkeeping; this
// one wires the real gateway, the real store and a real listener, because the
// claim "SMTP starts once a bundle applies" is not provable any other way.
func TestManaged_SMTPStartsAfterFirstDeploy(t *testing.T) {
	dataDir := t.TempDir()

	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	console := newConsole(t)
	srv := httptest.NewServer(console.handler())
	defer srv.Close()

	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	client := &central.Client{BaseURL: srv.URL, ID: "gw-1", Key: key, Log: discardLogger()}

	g := newGateway(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer g.shutdown()

	a := newAgent(st, "test", discardLogger())
	a.gw = g
	a.bundleOpts = config.BundleOptions{SpoolDir: filepath.Join(dataDir, "queue")}

	// Nothing deployed: no cache, so nothing to boot from and nothing serving.
	if boot := bootConfig(st, discardLogger()); boot != nil {
		t.Fatalf("a fresh gateway should have no cached configuration, got %+v", boot)
	}
	if g.Serving() {
		t.Fatal("a gateway with no configuration must not be listening")
	}

	// Deploy. Bind to a port the test can actually claim.
	addr := freeAddr(t)
	console.deploy(1, 1, "sha-v1", bundleListeningOn(t, addr))

	a.pull(ctx, client, status(console))

	if !g.Serving() {
		t.Fatal("the gateway should be serving after its first configuration applied")
	}
	// The real proof: something accepts a TCP connection on the deployed port.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("nothing is listening on the deployed address %s: %v", addr, err)
	}
	_ = conn.Close()

	// A restart now has something to come up on, without the console.
	boot := bootConfig(st, discardLogger())
	if boot == nil || boot.VersionID != 1 {
		t.Fatalf("boot would come up on %+v, want the deployed v1", boot)
	}
	if _, err := loadBundle(boot.Bundle, a.bundleOpts, BundleSource(boot)); err != nil {
		t.Fatalf("the cached bundle should load with the console unreachable: %v", err)
	}
}

// bundleListeningOn is the testdata bundle with its listen address replaced.
func bundleListeningOn(t *testing.T, addr string) []byte {
	t.Helper()

	var b config.Bundle
	if err := json.Unmarshal(bundleFromTestdata(t), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	server := "hostname: relay.test\nlisten:\n  - addr: \"" + addr + "\"\n"
	b.Server = &server

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// freeAddr reserves a port and releases it. Racy in principle; in practice the
// kernel does not hand the same ephemeral port out twice in the microseconds
// between here and the listener opening, and the alternative — plumbing a
// listener through the bundle — would test something other than the real path.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
