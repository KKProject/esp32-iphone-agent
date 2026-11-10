package daemon

import (
	"context"
	"errors"
	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/ble"
	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/config"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/proto"
)

func waitVia(t *testing.T, s *Server, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		s.mu.Lock()
		c := s.conns["246f28aa6ad5"]
		s.mu.Unlock()
		if c != nil && c.Via() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session never went to %s", want)
}

func waitNetworkOnly(t *testing.T, c *Conn) {
	t.Helper()
	for i := 0; i < 100; i++ {
		c.wifi.mu.Lock()
		done := c.wifi.configured
		c.wifi.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("network-only handoff was not acknowledged")
}

// Once provisioned, neither Wi-Fi failure nor a fresh daemon may scan/connect BLE.
type provisioningTransport struct {
	b        *proto.FakeBoard
	disabled atomic.Bool
	scans    atomic.Int32
	connects atomic.Int32
}

func (f *provisioningTransport) Scan(context.Context, time.Duration) ([]ble.Found, error) {
	f.scans.Add(1)
	if f.disabled.Load() {
		return nil, errors.New("BLE must not be scanned after provisioning")
	}
	return []ble.Found{{Name: "ESP32 Agent 6AD5", Addr: "fake"}}, nil
}
func (f *provisioningTransport) Connect(context.Context, ble.Found) (proto.Link, error) {
	f.connects.Add(1)
	if f.disabled.Load() {
		return nil, errors.New("BLE is off")
	}
	return f.b.Connect(), nil
}

func TestNetworkOnlyHandoffAndReconnect(t *testing.T) {
	b := fakeBoard()
	tr := &provisioningTransport{b: b}
	var mu sync.Mutex
	var wifi proto.Link
	dial := func(_ context.Context, ip string) (proto.Link, error) {
		if ip != "192.168.2.3" {
			return nil, errors.New("wrong address")
		}
		mu.Lock()
		defer mu.Unlock()
		wifi = b.Connect()
		return wifi, nil
	}
	client, s := startWith(t, tr, dial)
	s.WiFiCheck = 20 * time.Millisecond
	if r, _ := call(t, client, Request{Cmd: "pair"}); r.Status != "ok" {
		t.Fatal(r)
	}
	b.Status(`{"net":"sta","ip":"192.168.2.3"}`)
	waitVia(t, s, "wifi")
	conn := s.conns["246f28aa6ad5"]
	waitNetworkOnly(t, conn)
	conn.mu.Lock()
	bl := conn.bleLink
	conn.mu.Unlock()
	if bl != nil {
		t.Fatal("BLE retained after handoff")
	}
	tr.disabled.Store(true)
	scans, connects := tr.scans.Load(), tr.connects.Load()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkBoards("")[conn.ID].Address != "192.168.2.3" {
		t.Fatal("endpoint not persisted")
	}
	if r, _ := call(t, client, Request{Cmd: "keys", Text: "esc"}); r.Status != "ok" {
		t.Fatal(r)
	}
	mu.Lock()
	wifi.Close()
	mu.Unlock()
	waitVia(t, s, "")
	if r, _ := call(t, client, Request{Cmd: "keys", Text: "esc"}); r.Status != "ok" {
		t.Fatal(r)
	}
	if r, _ := call(t, client, Request{Cmd: "net", Text: "via ble"}); r.Status != "error" || !strings.Contains(r.Error, "BLE is disabled") {
		t.Fatal(r)
	}
	// Simulate a new daemon using the configuration reloaded from disk.
	conn.close()
	fresh := &Server{Transport: tr, Config: cfg, Dir: s.Dir, DialWiFi: dial, conns: map[string]*Conn{}}
	reopened, err := fresh.conn(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.Via() != "wifi" {
		t.Fatal("daemon restart did not use TCP")
	}
	if r, _ := call(t, client, Request{Cmd: "devices"}); r.Status != "ok" {
		t.Fatal(r)
	}
	if tr.scans.Load() != scans || tr.connects.Load() != connects {
		t.Fatal("used BLE after handoff")
	}
}

func TestNetworkFailureDoesNotFallBackToBLE(t *testing.T) {
	b := fakeBoard()
	tr := &provisioningTransport{b: b}
	client, s := start(t, tr)
	if r, _ := call(t, client, Request{Cmd: "pair"}); r.Status != "ok" {
		t.Fatal(r)
	}
	id := "246f28aa6ad5"
	if err := s.Config.SetNetwork(id, "192.168.2.3"); err != nil {
		t.Fatal(err)
	}
	s.conns[id].close()
	tr.disabled.Store(true)
	s.DialWiFi = func(context.Context, string) (proto.Link, error) { return nil, errors.New("timeout") }
	scans := tr.scans.Load()
	if r, _ := call(t, client, Request{Cmd: "info"}); r.Status != "error" || !strings.Contains(r.Error, "network-only") {
		t.Fatal(r)
	}
	if tr.scans.Load() != scans {
		t.Fatal("scanned BLE on network failure")
	}
	if r, _ := call(t, client, Request{Cmd: "net", Text: "address 192.168.2.8"}); r.Status != "ok" {
		t.Fatal(r)
	}
	if s.Config.NetworkBoards("")[id].Address != "192.168.2.8" {
		t.Fatal("address update failed")
	}
	if r, _ := call(t, client, Request{Cmd: "net", Text: "address invalid"}); r.Status != "error" {
		t.Fatal(r)
	}
}

func TestFailedBLEShutdownKeepsProvisioningLink(t *testing.T) {
	b := fakeBoard()
	b.Handle = func(cmd string) string {
		if cmd == "netonly on" {
			return "err unsupported"
		}
		return "ok"
	}
	client, s := startWith(t, fakeTransport{b}, func(context.Context, string) (proto.Link, error) { return b.Connect(), nil })
	s.WiFiCheck = 20 * time.Millisecond
	if r, _ := call(t, client, Request{Cmd: "pair"}); r.Status != "ok" {
		t.Fatal(r)
	}
	b.Status(`{"net":"sta","ip":"192.168.2.3"}`)
	waitVia(t, s, "wifi")
	conn := s.conns["246f28aa6ad5"]
	for i := 0; i < 100; i++ {
		_, e := conn.wifi.state()
		if e != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn.mu.Lock()
	bl := conn.bleLink
	conn.mu.Unlock()
	if bl == nil {
		t.Fatal("BLE closed before board acknowledged shutdown")
	}
	_, e := conn.wifi.state()
	if !strings.Contains(e, "updated firmware required") {
		t.Fatal(e)
	}
}

func TestRestoreSetupWithoutNetwork(t *testing.T) {
	b := fakeBoard()
	tr := &provisioningTransport{b: b}
	client, s := start(t, tr)
	if r, _ := call(t, client, Request{Cmd: "pair"}); r.Status != "ok" {
		t.Fatal(r)
	}
	id := "246f28aa6ad5"
	if err := s.Config.SetNetwork(id, "192.168.2.3"); err != nil {
		t.Fatal(err)
	}
	s.conns[id].close()
	tr.disabled.Store(true)
	s.DialWiFi = func(context.Context, string) (proto.Link, error) { return nil, errors.New("offline") }
	if r, d := call(t, client, Request{Cmd: "net", Text: "setup"}); r.Status != "ok" || d["board_restored"] != false || !strings.Contains(d["message"].(string), "0 setup") {
		t.Fatal(r)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.NetworkBoards("")) != 0 {
		t.Fatal("network-only setting retained")
	}
	if cfg.Key(mustHex(id)) == nil {
		t.Fatal("setup erased pairing key")
	}
}

func TestRestoreSetupOverNetwork(t *testing.T) {
	b := fakeBoard()
	client, s := startWith(t, fakeTransport{b}, func(context.Context, string) (proto.Link, error) { return b.Connect(), nil })
	s.WiFiCheck = 20 * time.Millisecond
	if r, _ := call(t, client, Request{Cmd: "pair"}); r.Status != "ok" {
		t.Fatal(r)
	}
	b.Status(`{"net":"sta","ip":"192.168.2.3"}`)
	waitVia(t, s, "wifi")
	conn := s.conns["246f28aa6ad5"]
	waitNetworkOnly(t, conn)
	if r, d := call(t, client, Request{Cmd: "net", Text: "setup"}); r.Status != "ok" || d["board_restored"] != true {
		t.Fatal(r)
	}
	if len(s.Config.NetworkBoards("")) != 0 {
		t.Fatal("network-only setting retained")
	}
}
