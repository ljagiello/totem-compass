package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/client/clienttest"
	"github.com/ljagiello/totem-compass/protocol"
)

// syncBuffer is a bytes.Buffer safe for the client's goroutines to log into.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness runs totemctl command lines against a simulated Totem.
type harness struct {
	totem *clienttest.Totem
	g     *globals
	out   bytes.Buffer
	errb  syncBuffer
	dials int
}

func newHarness() *harness {
	h := &harness{totem: clienttest.New()}
	// Commands return as soon as the simulator answers; the scale only bounds
	// how long a failing command waits (30 s device timeouts become 3 s).
	h.g = &globals{start: time.Now(), timeScale: 0.1}
	h.g.connect = func(_ context.Context, opts client.Options) (*client.Client, error) {
		h.dials++
		return h.totem.Connect(opts)
	}
	return h
}

func (h *harness) run(t *testing.T, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := newRootCmd(h.g)
	root.SetArgs(args)
	root.SetOut(&h.out)
	root.SetErr(&h.errb)
	return root.ExecuteContext(ctx)
}

func (h *harness) mustRun(t *testing.T, args ...string) {
	t.Helper()
	if err := h.run(t, args...); err != nil {
		t.Fatalf("totemctl %s: %v\nstderr:\n%s", strings.Join(args, " "), err, h.errb.String())
	}
}

// state reads the simulated device state safely.
func (h *harness) state() (st protocol.StaticData, live protocol.LiveData, peers []protocol.PeerPing, blink bool) {
	h.totem.Do(func(t *clienttest.Totem) {
		st, live, peers, blink = t.Static, t.Live, append([]protocol.PeerPing(nil), t.Peers...), t.PeerBlink
	})
	return
}

func wrote(h *harness, f protocol.Frame) bool {
	for _, got := range h.totem.Received() {
		if got.Channel == f.Channel && bytes.Equal(got.Bytes, f.Bytes) {
			return true
		}
	}
	return false
}

var (
	peerA = protocol.MAC{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6}
	peerB = protocol.MAC{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
)

func withPeers(h *harness) {
	h.totem.Peers = []protocol.PeerPing{
		{MAC: peerA, Name: "Zed", Lat: 52.25, Lon: 13.5, Bearing: 90, RSSI: -60, BattPct: 64, Color: protocol.RGB{R: 255}},
		{MAC: peerB, Name: "Amy", Bearing: -1, RSSI: 100, BattPct: 80, Color: protocol.RGB{B: 255}},
	}
}

func TestHelp(t *testing.T) {
	h := newHarness()
	h.mustRun(t) // no command prints help
	for _, want := range []string{"Available Commands:", "compass", "location", "double-press the power button", "--half-duplex"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, h.out.String())
		}
	}
	if h.dials != 0 {
		t.Error("help connected to the Totem")
	}
}

func TestTraceLogsFramesAndPhases(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "--trace", "info")
	for _, want := range []string{
		"level=DEBUG msg=frame dir=tx channel=conn_status data=00010100", // Ready
		"level=DEBUG msg=frame dir=rx channel=data data=0102",            // Static Data
		`level=DEBUG msg="handshake sent" elapsed=`,
		`level=DEBUG msg=disconnected elapsed=`,
	} {
		if !strings.Contains(h.errb.String(), want) {
			t.Errorf("trace lacks %q:\n%s", want, h.errb.String())
		}
	}
}

// Global settings come from a flag, else TOTEM_*, else the config file.
func TestConfigFileEnvAndFlags(t *testing.T) {
	for _, k := range []string{"TOTEM_DEVICE", "TOTEM_SCAN_TIMEOUT", "TOTEM_JSON"} {
		t.Setenv(k, "") // empty counts as unset
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("device: from-file\nscan-timeout: 5s\njson: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness()
	h.mustRun(t, "--config", path, "info")
	if h.g.device != "from-file" || h.g.scanTimeout != 5*time.Second {
		t.Errorf("from the file: device %q, scan timeout %v", h.g.device, h.g.scanTimeout)
	}
	if !strings.HasPrefix(h.out.String(), `{"type":"StaticData"`) {
		t.Errorf("json: true in the file, but the output is:\n%s", h.out.String())
	}

	t.Setenv("TOTEM_DEVICE", "from-env")
	h = newHarness()
	h.mustRun(t, "--config", path, "info")
	if h.g.device != "from-env" {
		t.Errorf("TOTEM_DEVICE set: device %q", h.g.device)
	}
	h = newHarness()
	h.mustRun(t, "--config", path, "-d", "from-flag", "info")
	if h.g.device != "from-flag" {
		t.Errorf("-d given: device %q", h.g.device)
	}

	// The default config file may be missing; one asked for may not.
	missing := filepath.Join(t.TempDir(), "config.yaml")
	h = newHarness()
	h.g.configPath = missing
	h.mustRun(t, "info")
	h = newHarness()
	if err := h.run(t, "--config", missing, "info"); err == nil || h.dials != 0 {
		t.Errorf("missing --config file: err %v, dials %d", err, h.dials)
	}
}

func TestInfo(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "info")
	for _, want := range []string{"Test Totem", "5.0.3 (release 339, branch totem)", "97% (4.10 V)", "normal", "37.586700, -122.0073", "Half duplex       supported"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("info output lacks %q:\n%s", want, h.out.String())
		}
	}
	for _, f := range []protocol.Frame{protocol.Ready(protocol.SchemaLegacy), protocol.AckStaticData(), protocol.DisconnectRequest()} {
		if !wrote(h, f) {
			t.Errorf("totemctl never wrote % x", f.Bytes)
		}
	}
}

func TestInfoJSON(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "--json", "info")
	var types []string
	sc := bufio.NewScanner(&h.out)
	for sc.Scan() {
		var line struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("not a JSON line: %q: %v", sc.Text(), err)
		}
		types = append(types, line.Type)
	}
	if strings.Join(types, ",") != "StaticData,LiveData" {
		t.Errorf("JSON record types = %v", types)
	}
}

func TestCompassMergesCurrentSettings(t *testing.T) {
	h := newHarness()
	h.totem.Static.PersistentNorth = true
	h.mustRun(t, "compass", "--lock", "on")
	st, live, _, blink := h.state()
	if !st.CompassLock || !st.PersistentNorth {
		t.Errorf("lock %v north %v, want both on (north must be preserved)", st.CompassLock, st.PersistentNorth)
	}
	if blink || live.PowerMode != protocol.PowerNormal {
		t.Errorf("blink %v power %s, want off / unchanged", blink, live.PowerMode)
	}
	if !strings.Contains(h.errb.String(), `level=WARN msg="the Totem does not report peer blink`) {
		t.Errorf("no peer-blink warning:\n%s", h.errb.String())
	}
}

func TestCompassBlinkNeedsNoWarning(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "compass", "--blink", "on", "--north=on", "--power", "eco")
	st, live, _, blink := h.state()
	if !blink || !st.PersistentNorth || st.CompassLock || live.PowerMode != protocol.PowerEco {
		t.Errorf("blink %v north %v lock %v power %s", blink, st.PersistentNorth, st.CompassLock, live.PowerMode)
	}
	if strings.Contains(h.errb.String(), "level=WARN") {
		t.Errorf("unexpected warning:\n%s", h.errb.String())
	}
}

func TestPower(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "power", "eco")
	if _, live, _, _ := h.state(); live.PowerMode != protocol.PowerEco {
		t.Errorf("power mode %s, want eco", live.PowerMode)
	}
	if !strings.Contains(h.errb.String(), "power eco") {
		t.Errorf("status:\n%s", h.errb.String())
	}
}

func TestPowerOnFirmwareThatDoesNotReportIt(t *testing.T) {
	h := newHarness()
	h.totem.HidePowerMode = true // firmware 4.x
	h.mustRun(t, "power", "eco")
	if !strings.Contains(h.errb.String(), `level=WARN msg="this firmware does not report its power mode`) {
		t.Errorf("no unconfirmed-power warning:\n%s", h.errb.String())
	}
	if _, live, _, _ := h.state(); live.PowerMode != protocol.PowerEco {
		t.Errorf("power mode %s, want eco (the command must still be sent)", live.PowerMode)
	}
}

func TestName(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "name", "Base", "Camp")
	if st, _, _, _ := h.state(); st.Name != "Base Camp" {
		t.Errorf("name %q", st.Name)
	}
}

func TestWiFi(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "wifi", "scan")
	if got := h.out.String(); got != "home\ncafe\n" {
		t.Errorf("scan output %q", got)
	}

	h = newHarness()
	h.mustRun(t, "wifi", "set", "Domek", "s3cret")
	var ssid, key string
	h.totem.Do(func(t *clienttest.Totem) { ssid, key = t.Static.WiFiSSID, t.WiFiKey })
	if ssid != "Domek" || key != "s3cret" {
		t.Errorf("saved %q / %q", ssid, key)
	}
}

func TestPeers(t *testing.T) {
	h := newHarness()
	withPeers(h)
	h.mustRun(t, "peers")
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"Amy"`) || !strings.Contains(lines[1], `"Zed"`) {
		t.Fatalf("peers output, want Amy then Zed:\n%s", h.out.String())
	}
	if !strings.Contains(lines[1], "52.250000,13.500000") || !strings.Contains(lines[1], "rssi -60") {
		t.Errorf("Zed's line lacks position or RSSI: %s", lines[1])
	}

	h = newHarness()
	h.mustRun(t, "peers")
	if !strings.Contains(h.errb.String(), "no bonded peers") {
		t.Errorf("empty peer list:\n%s", h.errb.String())
	}
}

func TestPeerCommands(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		check func(t *testing.T, peers []protocol.PeerPing)
	}{
		{[]string{"color", peerA.String(), "hot_pink"}, func(t *testing.T, p []protocol.PeerPing) {
			if p[0].Color != (protocol.RGB{R: 255, B: 128}) {
				t.Errorf("colour %s", p[0].Color)
			}
		}},
		{[]string{"hide", peerA.Pretty()}, func(t *testing.T, p []protocol.PeerPing) {
			if !p[0].Hidden || p[0].Color != (protocol.RGB{R: 255}) {
				t.Errorf("hidden %v colour %s (colour must be kept)", p[0].Hidden, p[0].Color)
			}
		}},
		{[]string{"delete", peerA.String()}, func(t *testing.T, p []protocol.PeerPing) {
			if len(p) != 1 || p[0].MAC != peerB {
				t.Errorf("peers after delete: %v", p)
			}
		}},
	} {
		t.Run(tc.args[0], func(t *testing.T) {
			h := newHarness()
			withPeers(h)
			h.mustRun(t, append([]string{"peer"}, tc.args...)...)
			_, _, peers, _ := h.state()
			tc.check(t, peers)
		})
	}
}

func TestPeerSelect(t *testing.T) {
	h := newHarness()
	withPeers(h)
	h.mustRun(t, "peer", "select", peerA.String())
	if !wrote(h, protocol.SelectPeer(peerA)) {
		t.Error("select did not send (6,5)")
	}
	h = newHarness()
	h.mustRun(t, "peer", "select", "--stop")
	if !wrote(h, protocol.StopPeerManagement()) {
		t.Error("select --stop did not send (6,0)")
	}
}

func TestPeerUnknown(t *testing.T) {
	h := newHarness()
	h.g.timeScale = 0.01 // this one waits out the timeout
	err := h.run(t, "peer", "hide", "0a0b0c0d0e0f")
	if err == nil || !strings.Contains(err.Error(), "no peer 0a0b0c0d0e0f") {
		t.Errorf("err = %v", err)
	}
}

func TestPOI(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "poi", "add", "--name", "Main Stage", "--lat", "50.0671", "--lon", "19.9124", "--color", "orange", "--id", "02aabbccddee")
	_, _, peers, _ := h.state()
	if len(peers) != 1 {
		t.Fatalf("peers %v", peers)
	}
	p := peers[0]
	if !p.POI || p.Name != "Main Stage" || p.Color != (protocol.RGB{R: 255, G: 128}) || p.MAC.String() != "02aabbccddee" {
		t.Errorf("POI %+v", p)
	}
	if !strings.HasPrefix(h.out.String(), "added 02aabbccddee poi") {
		t.Errorf("output %q", h.out.String())
	}

	h = newHarness()
	h.mustRun(t, "poi", "add", "--name", "Camp", "--lat", "-33.8568", "--lon", "-70.6483")
	_, _, peers, _ = h.state()
	if len(peers) != 1 || peers[0].MAC[0]&0x03 != 0x02 {
		t.Errorf("random POI id must be unicast and locally administered: %v", peers)
	}
	if peers[0].Lat > -33.85 || peers[0].Lon > -70.64 {
		t.Errorf("POI at %v,%v, want -33.8568,-70.6483", peers[0].Lat, peers[0].Lon)
	}
}

func TestLocation(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "location", "--lat", "50.0671", "--lon", "-19.9124", "--acc", "5")
	var fix *protocol.PhoneFix
	h.totem.Do(func(t *clienttest.Totem) { fix = t.Fix })
	if fix == nil || fix.Lat < 50.067 || fix.Lat > 50.0672 || fix.Lon > -19.912 || fix.HAcc != 5 || !fix.Focused {
		t.Fatalf("fix %+v", fix)
	}
	if d := time.Since(time.Unix(int64(fix.Unix), 0)); d < 0 || d > time.Minute {
		t.Errorf("fix clock is %v off", d)
	}
}

func TestOTA(t *testing.T) {
	h := newHarness()
	if err := h.run(t, "ota"); err == nil || h.dials != 0 {
		t.Fatalf("ota without --yes: err %v, dials %d", err, h.dials)
	}
	h.mustRun(t, "ota", "--yes")
	if !strings.Contains(h.errb.String(), "reboot into the updater") {
		t.Errorf("status:\n%s", h.errb.String())
	}
}

func TestWatchAndRaw(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "watch", "--for", "150ms")
	for _, want := range []string{" static \"Test Totem\"", " live   37.586700,-122.0073"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("watch output lacks %q:\n%s", want, h.out.String())
		}
	}

	h = newHarness()
	h.mustRun(t, "raw", "01 01", "--listen", "150ms") // flags may follow arguments
	if !strings.Contains(h.out.String(), " static ") {
		t.Errorf("raw output:\n%s", h.out.String())
	}
}

func TestBadArgumentsFailBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"frobnicate"},
		{"--no-such-flag", "info"},
		{"info", "extra"},
		{"name"},
		{"compass"},
		{"compass", "--lock", "maybe"},
		{"compass", "--power", "turbo"},
		{"power"},
		{"power", "turbo"},
		{"wifi"},
		{"wifi", "frobnicate"},
		{"wifi", "set"},
		{"peer"},
		{"peer", "frobnicate", "a1b2c3d4e5f6"},
		{"peer", "color", "a1b2c3d4e5f6"},
		{"peer", "color", "a1b2c3d4e5f6", "not-a-colour"},
		{"peer", "hide", "a1b2"},
		{"peer", "select"},
		{"peer", "select", "--stop", "a1b2c3d4e5f6"},
		{"poi", "add", "--lat", "1", "--lon", "2"},
		{"poi", "add", "--name", "x", "--lat", "91", "--lon", "2"},
		{"poi", "delete"},
		{"location", "--lat", "50"},
		{"location", "--lat", "91", "--lon", "0"},
		{"location", "--lat", "north", "--lon", "west"},
		{"raw", "01"},
		{"raw", "zz"},
	} {
		h := newHarness()
		if err := h.run(t, args...); err == nil {
			t.Errorf("totemctl %s: no error", strings.Join(args, " "))
		}
		if h.dials != 0 {
			t.Errorf("totemctl %s: connected before rejecting its arguments", strings.Join(args, " "))
		}
	}
}

func TestParseOnOffAndPower(t *testing.T) {
	for in, want := range map[string]bool{"on": true, "Yes": true, "1": true, "off": false, "FALSE": false} {
		if got, err := parseOnOff(in); err != nil || got != want {
			t.Errorf("parseOnOff(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseOnOff("maybe"); err == nil {
		t.Error("parseOnOff accepted maybe")
	}
	for in, want := range map[string]protocol.PowerMode{"eco": protocol.PowerEco, "NORMAL": protocol.PowerNormal, "full": protocol.PowerNormal} {
		if got, err := parsePower(in); err != nil || got != want {
			t.Errorf("parsePower(%q) = %v, %v", in, got, err)
		}
	}
}
