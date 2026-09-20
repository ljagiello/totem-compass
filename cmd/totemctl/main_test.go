package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// TestMain drops TOTEM_* variables from the developer's environment, which
// viper would otherwise read into every test.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "TOTEM_") {
			_ = os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

// harness runs totemctl command lines against a simulated Totem.
type harness struct {
	totem *clienttest.Totem
	g     *globals
	stdin string
	in    io.Reader // stdin, if not the string
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
	return h.runCtx(ctx, args...)
}

func (h *harness) runCtx(ctx context.Context, args ...string) error {
	root := newRootCmd(h.g)
	root.SetArgs(args)
	root.SetIn(strings.NewReader(h.stdin))
	if h.in != nil {
		root.SetIn(h.in)
	}
	root.SetOut(&h.out)
	root.SetErr(&h.errb)
	return root.ExecuteContext(ctx)
}

// session opens a session on the simulated Totem, as a command would.
func (h *harness) session(t *testing.T) *session {
	t.Helper()
	h.g.out = &printer{out: &h.out, err: &h.errb}
	h.g.setLogger(newLogger(&h.errb, false))
	s, err := open(context.Background(), h.g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
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
	h.mustRun(t, "compass", "--lock", "on", "--blink", "off")
	st, live, _, blink := h.state()
	if !st.CompassLock || !st.PersistentNorth {
		t.Errorf("lock %v north %v, want both on (north must be preserved)", st.CompassLock, st.PersistentNorth)
	}
	if blink || live.PowerMode != protocol.PowerNormal {
		t.Errorf("blink %v power %s, want off / unchanged", blink, live.PowerMode)
	}
}

// The (7,3) frame always sets peer blink, which the Totem does not report:
// without a value from the user, compass and power used to turn it off.
func TestBlinkMustBeKnown(t *testing.T) {
	for _, args := range [][]string{{"compass", "--lock", "on"}, {"power", "eco"}} {
		h := newHarness()
		h.totem.PeerBlink = true
		err := h.run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "--blink on or --blink off") || h.dials != 0 {
			t.Errorf("%v: err %v, dials %d", args, err, h.dials)
		}
	}

	// TOTEM_BLINK and the config file supply it too.
	t.Setenv("TOTEM_BLINK", "on")
	h := newHarness()
	h.mustRun(t, "power", "eco")
	if _, live, _, blink := h.state(); !blink || live.PowerMode != protocol.PowerEco {
		t.Errorf("TOTEM_BLINK=on: blink %v power %s", blink, live.PowerMode)
	}
	t.Setenv("TOTEM_BLINK", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("blink: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h = newHarness()
	h.mustRun(t, "--config", path, "compass", "--lock", "on")
	if _, _, _, blink := h.state(); !blink {
		t.Error("blink: on in the config file was not used")
	}
	h = newHarness()
	h.mustRun(t, "--config", path, "compass", "--lock", "on", "--blink", "off")
	if _, _, _, blink := h.state(); blink {
		t.Error("--blink off did not beat the config file")
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
	h.mustRun(t, "power", "eco", "--blink", "on")
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
	h.mustRun(t, "power", "eco", "--blink", "off")
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
	// The device strips the name; the confirmation must expect that, not
	// wait for the name as typed.
	h = newHarness()
	h.mustRun(t, "name", "  Dune  ")
	if st, _, _, _ := h.state(); st.Name != "Dune" {
		t.Errorf("name %q", st.Name)
	}
}

func TestWiFi(t *testing.T) {
	h := newHarness()
	h.mustRun(t, "wifi", "scan")
	if got := h.out.String(); got != "home\ncafe\n" {
		t.Errorf("scan output %q", got)
	}

	// The password comes from standard input, never the command line, so
	// one starting with '-' is not taken for flags.
	h = newHarness()
	h.stdin = "-s3cret\n"
	h.mustRun(t, "wifi", "set", "Domek")
	var ssid, key string
	h.totem.Do(func(t *clienttest.Totem) { ssid, key = t.Static.WiFiSSID, t.WiFiKey })
	if ssid != "Domek" || key != "-s3cret" {
		t.Errorf("saved %q / %q", ssid, key)
	}

	h = newHarness()
	h.mustRun(t, "wifi", "set", "Cafe", "--open")
	h.totem.Do(func(t *clienttest.Totem) { ssid, key = t.Static.WiFiSSID, t.WiFiKey })
	if ssid != "Cafe" || key != "" {
		t.Errorf("open network saved %q / %q", ssid, key)
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
	if !errors.Is(err, errNoPeer) || !strings.Contains(err.Error(), "0a0b0c0d0e0f") {
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
		{"name", "   "},
		{"name", strings.Repeat("x", 33)},
		{"wifi", "set", strings.Repeat("s", 33)},
		{"poi", "add", "--name", strings.Repeat("p", 128), "--lat", "1", "--lon", "2"},
		{"compass"},
		{"compass", "--lock", "on"}, // peer blink unknown
		{"power", "eco"},
		{"compass", "--lock", "maybe"},
		{"compass", "--power", "turbo"},
		{"power"},
		{"power", "turbo", "--blink", "on"},
		{"wifi"},
		{"wifi", "frobnicate"},
		{"wifi", "set"},
		{"wifi", "set", "Home", "password"},
		{"wifi", "set", "Home"}, // no password on stdin and no --open
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
		{"location", "--lat", "NaN", "--lon", "0"},
		{"location", "--lat", "1", "--lon", "1", "--acc", "NaN"},
		{"location", "--lat", "1", "--lon", "1", "--acc", "-5"},
		{"poi", "add", "--name", "x", "--lat", "NaN", "--lon", "1"},
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

// FuzzParseRawFrame checks the hex parser behind `totemctl raw`.
// TestCleanHex: hex is pasted from documents that hold non-breaking and
// thin spaces, and separated with colons.
func TestCleanHex(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"01 01", "0101"},
		{"00:01:01:00", "00010100"},
		{"01\u00a001", "0101"},   // a non-breaking space
		{"01\u200601", "0101"},   // a thin space
		{"01\t01\n02", "010102"}, // tabs and newlines
		{"0101", "0101"},
	} {
		if got := cleanHex(tt.in); got != tt.want {
			t.Errorf("cleanHex(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func FuzzParseRawFrame(f *testing.F) {
	for _, s := range []string{"01 01", "0101", "00:01:01:00", "01", "zz", "", "0 1 0 1", "ABCDEF"} {
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, s string, conn bool) {
		// The shell hands the command its arguments already split, so the
		// model starts where parseRawFrame does: with those arguments
		// joined. Splitting can even rejoin the bytes of a space that was
		// split through the middle.
		args := strings.Fields(s)
		fr, err := parseRawFrame(args, conn)
		if err != nil {
			return
		}
		clean := strings.ToLower(cleanHex(strings.Join(args, "")))
		if len(fr.Bytes) < 2 || hex.EncodeToString(fr.Bytes) != clean {
			t.Fatalf("parseRawFrame(%q) = % x, want the bytes of %q", s, fr.Bytes, clean)
		}
		if (fr.Channel == protocol.ConnStatus) != conn {
			t.Fatalf("parseRawFrame(%q, %v) wrote to %s", s, conn, fr.Channel)
		}
	})
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

// A timeout computed from a deadline that has passed must fail at once, not
// wait for good: fetchStatic, after a slow send, used to hang.
func TestExpiredTimeoutsDoNotWaitForever(t *testing.T) {
	h := newHarness()
	s := h.session(t)
	never := func(protocol.Message) bool { return false }
	for _, d := range []time.Duration{0, -time.Second} {
		start := time.Now()
		if _, err := s.await(d, never); !errors.Is(err, errTimeout) || time.Since(start) > 100*time.Millisecond {
			t.Errorf("await(%v) = %v after %v", d, err, time.Since(start))
		}
	}
	start := time.Now()
	_, err := s.fetchStatic(0, func(protocol.StaticData) bool { return false })
	if !errors.Is(err, errTimeout) || time.Since(start) > time.Second {
		t.Errorf("fetchStatic past its deadline = %v after %v", err, time.Since(start))
	}
}

// --half-duplex on firmware without the half-duplex loop (4.x) fails fast
// with an explanation, instead of every send waiting 30 s for a TX window.
func TestHalfDuplexNeedsSupport(t *testing.T) {
	h := newHarness()
	h.totem.Static.HalfDuplex = false
	start := time.Now()
	err := h.run(t, "--half-duplex", "info")
	if err == nil || !strings.Contains(err.Error(), "run without --half-duplex") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v", d)
	}
}

// Ctrl-C ends watch normally; anything else it cuts short exits 130.
func TestInterrupt(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := h.runCtx(ctx, "watch"); err != nil {
		t.Errorf("watch ended by Ctrl-C: %v", err)
	}
	for _, tc := range []struct {
		err, cause error
		want       int
	}{
		{nil, nil, 0},
		{errors.New("x"), nil, 1},
		{context.Canceled, stoppedBy{os.Interrupt}, 130},
		{fmt.Errorf("send: %w", context.Canceled), stoppedBy{syscall.SIGTERM}, 143},
		{context.Canceled, nil, 130},
	} {
		if got := exitCode(tc.err, tc.cause); got != tc.want {
			t.Errorf("exitCode(%v, %v) = %d, want %d", tc.err, tc.cause, got, tc.want)
		}
	}
}

// Ctrl-C while info waits for Live Data used to print a partial result and
// exit 0; a timeout still prints what there is.
func TestInfoInterrupted(t *testing.T) {
	h := newHarness()
	h.totem.NoLiveData = true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if err := h.runCtx(ctx, "info"); !errors.Is(err, context.Canceled) {
		t.Errorf("interrupted info: %v", err)
	}
	if h.out.Len() != 0 {
		t.Errorf("interrupted info printed:\n%s", h.out.String())
	}

	h = newHarness()
	h.totem.NoLiveData = true
	h.mustRun(t, "info")
	if !strings.Contains(h.out.String(), "Test Totem") {
		t.Errorf("info without Live Data:\n%s", h.out.String())
	}
}

// `poi add --id` with a bonded Totem's MAC would replace the bond with a
// POI; an existing POI's id is fine (it updates the POI).
func TestPOIAddNeverReplacesABond(t *testing.T) {
	h := newHarness()
	withPeers(h)
	err := h.run(t, "poi", "add", "--name", "x", "--lat", "1", "--lon", "2", "--id", peerA.String())
	if err == nil || !strings.Contains(err.Error(), "would replace the bond") {
		t.Errorf("POI over a bond: %v", err)
	}
	if _, _, peers, _ := h.state(); peers[0].POI || peers[0].Name != "Zed" {
		t.Errorf("the bond was replaced: %+v", peers[0])
	}

	h = newHarness()
	h.totem.Peers = []protocol.PeerPing{{MAC: protocol.MAC{2, 0xaa}, Name: "Old", POI: true, RSSI: 100}}
	h.mustRun(t, "poi", "add", "--name", "New", "--lat", "1", "--lon", "2", "--id", "02aa00000000")
	if _, _, peers, _ := h.state(); len(peers) != 1 || peers[0].Name != "New" {
		t.Errorf("POI not updated: %+v", peers)
	}
}

// halfDuplexTotem answers Ready with one TX handoff and then stays silent,
// never handing TX back after the client grants it.
type halfDuplexTotem struct {
	mu   sync.Mutex
	subs map[protocol.Channel]func([]byte)
	done chan struct{}
	once sync.Once
}

func (q *halfDuplexTotem) Subscribe(ch protocol.Channel, fn func([]byte)) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.subs[ch] = fn
	return nil
}

func (q *halfDuplexTotem) Write(ch protocol.Channel, b []byte) error {
	if ch == protocol.ConnStatus && b[0] == protocol.CatConn && b[1] == 0x01 {
		q.mu.Lock()
		fn := q.subs[protocol.ConnStatus]
		q.mu.Unlock()
		go fn([]byte{protocol.CatHandoff, 0x02, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	}
	return nil
}

func (q *halfDuplexTotem) Done() <-chan struct{} { return q.done }

func (q *halfDuplexTotem) Close() error {
	q.once.Do(func() { close(q.done) })
	return nil
}

// Waiting for the TX window must end at the caller's deadline: each send
// used to have its own 30 s, so a 30 s confirmation could take a minute.
func TestSendRespectsCallerDeadline(t *testing.T) {
	h := newHarness()
	h.g.halfDuplex = true
	q := &halfDuplexTotem{subs: map[protocol.Channel]func([]byte){}, done: make(chan struct{})}
	h.g.connect = func(_ context.Context, opts client.Options) (*client.Client, error) {
		opts.GrantDelay = time.Millisecond
		return client.New(q, opts)
	}
	s := h.session(t)
	time.Sleep(50 * time.Millisecond) // AutoGrant hands TX to the device for good
	start := time.Now()
	_, err := s.fetchStatic(300*time.Millisecond, nil)
	if !errors.Is(err, errTimeout) {
		t.Errorf("fetchStatic = %v, want a timeout", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a 300 ms fetch took %v (the send waited on its own timeout)", d)
	}
}

// viper reads a bare number from the environment or a config file as a
// duration in ns: `scan-timeout: 30` made every scan end at once. It is
// rejected now, like anything under a second.
func TestScanTimeoutSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"30s": 30 * time.Second, "1.5s": 1500 * time.Millisecond, "2m": 2 * time.Minute} {
		t.Setenv("TOTEM_SCAN_TIMEOUT", in)
		h := newHarness()
		h.mustRun(t, "info")
		if h.g.scanTimeout != want {
			t.Errorf("TOTEM_SCAN_TIMEOUT=%s: %v, want %v", in, h.g.scanTimeout, want)
		}
	}
	for _, in := range []string{"30", "0", "5ms", "-3s", "soon"} {
		t.Setenv("TOTEM_SCAN_TIMEOUT", in)
		h := newHarness()
		if err := h.run(t, "info"); err == nil || !strings.Contains(err.Error(), "scan-timeout") || h.dials != 0 {
			t.Errorf("TOTEM_SCAN_TIMEOUT=%s: err %v, dials %d", in, err, h.dials)
		}
	}
}

// A mistyped MAC is rejected from the peer list in one round trip, not
// after a fetchPeer timeout.
func TestUnknownPeerFailsFast(t *testing.T) {
	for _, args := range [][]string{
		{"peer", "hide", "0a0b0c0d0e0f"},
		{"peer", "select", "0a0b0c0d0e0f"},
		{"poi", "delete", "0a0b0c0d0e0f"},
	} {
		h := newHarness()
		withPeers(h)
		start := time.Now()
		err := h.run(t, args...)
		if !errors.Is(err, errNoPeer) {
			t.Errorf("%v: %v", args, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%v took %v", args, d)
		}
	}
}

// A peer edit's result is output (stdout, JSON with --json), not progress.
func TestPeerEditPrintsResult(t *testing.T) {
	h := newHarness()
	withPeers(h)
	h.mustRun(t, "--json", "peer", "color", peerA.String(), "blue")
	var line struct {
		Type string
		Data struct{ MAC string }
	}
	if err := json.Unmarshal(h.out.Bytes(), &line); err != nil || line.Type != "PeerPing" || line.Data.MAC != peerA.String() {
		t.Errorf("--json peer color printed %q (%v)", h.out.String(), err)
	}

	h = newHarness()
	withPeers(h)
	h.mustRun(t, "-q", "peer", "hide", peerA.String())
	if !strings.Contains(h.out.String(), "hidden") {
		t.Errorf("-q hid the result: %q", h.out.String())
	}
}

// Peers with the same name come out in one order, by MAC.
func TestPeersOrderIsStable(t *testing.T) {
	for range 5 {
		h := newHarness()
		h.totem.Peers = []protocol.PeerPing{
			{MAC: protocol.MAC{2, 3}, Name: "Stage", POI: true, RSSI: 100},
			{MAC: protocol.MAC{2, 1}, Name: "Stage", POI: true, RSSI: 100},
			{MAC: protocol.MAC{2, 2}, Name: "Stage", POI: true, RSSI: 100},
		}
		h.mustRun(t, "peers")
		lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[0], "0201") || !strings.HasPrefix(lines[1], "0202") || !strings.HasPrefix(lines[2], "0203") {
			t.Fatalf("order:\n%s", h.out.String())
		}
	}
}

// settle waits for Live Data the device sent after the command; one still
// queued from before used to count, so deletes reported success at once.
func TestSettleIgnoresQueuedLiveData(t *testing.T) {
	h := newHarness()
	s := h.session(t)
	time.Sleep(50 * time.Millisecond) // Live Data piles up unread
	if err := s.send(protocol.StopPeerManagement()); err != nil {
		t.Fatal(err)
	}
	if err := s.settle(); err != nil {
		t.Fatal(err)
	}
	if !s.eventAt.After(s.sentAt) {
		t.Errorf("settle returned on Live Data from %v, before the send at %v", s.eventAt, s.sentAt)
	}
}

// viper's GetBool read TOTEM_JSON=yes or quiet: on as false, silently.
func TestSwitchSettingsTakeOnOff(t *testing.T) {
	t.Setenv("TOTEM_JSON", "yes")
	h := newHarness()
	h.mustRun(t, "info")
	if !strings.HasPrefix(h.out.String(), `{"type":"StaticData"`) {
		t.Errorf("TOTEM_JSON=yes: %q", h.out.String())
	}
	t.Setenv("TOTEM_JSON", "")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("quiet: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h = newHarness()
	h.mustRun(t, "--config", path, "power", "eco", "--blink", "on")
	if strings.Contains(h.errb.String(), "power eco") {
		t.Errorf("quiet: on still printed progress:\n%s", h.errb.String())
	}

	t.Setenv("TOTEM_TRACE", "maybe")
	h = newHarness()
	if err := h.run(t, "info"); err == nil || !strings.Contains(err.Error(), "trace setting") || h.dials != 0 {
		t.Errorf("TOTEM_TRACE=maybe: err %v, dials %d", err, h.dials)
	}
}

// A delete is confirmed by the peer list, not assumed.
func TestDeletesAreConfirmed(t *testing.T) {
	h := newHarness()
	withPeers(h)
	h.totem.IgnoreDeletes = true
	err := h.run(t, "peer", "delete", peerA.String())
	if err == nil || !strings.Contains(err.Error(), "still lists") {
		t.Errorf("an ignored delete: %v", err)
	}
	if strings.Contains(h.errb.String(), "deleted") {
		t.Errorf("reported a delete that did not happen:\n%s", h.errb.String())
	}
}

// Ctrl-C at the password prompt returns at once (and, on a terminal, puts
// echo back), instead of waiting for input the second Ctrl-C would kill.
func TestPasswordPromptIsInterruptible(t *testing.T) {
	h := newHarness()
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	h.in = pr // stdin that never delivers a line
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	err := h.runCtx(ctx, "wifi", "set", "Home")
	if !errors.Is(err, context.Canceled) || h.dials != 0 {
		t.Errorf("err %v, dials %d", err, h.dials)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
}

// scan prints each Totem once, with the name that arrived after its first
// advertisement; it printed both reports.
func TestScanPrintsEachTotemOnce(t *testing.T) {
	h := newHarness()
	h.g.scan = func(_ context.Context, _ bool, found func(client.Device)) error {
		found(client.Device{RSSI: -60})                // service UUID only
		found(client.Device{Name: "totem", RSSI: -58}) // then the scan response
		return nil
	}
	h.mustRun(t, "--json", "scan")
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"name":"totem"`) {
		t.Errorf("scan output:\n%s", h.out.String())
	}
}

// A ping this session already has is reused, not requested again.
func TestFetchPeerReusesAPingItHas(t *testing.T) {
	h := newHarness()
	withPeers(h)
	s := h.session(t)
	if _, err := s.peerList(); err != nil { // its ack asks for every ping
		t.Fatal(err)
	}
	if _, err := s.await(time.Second, func(protocol.Message) bool { _, ok := s.peers[peerA]; return ok }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.fetchPeer(peerA, time.Second, nil); err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.RequestPeerDetails(peerA)
	if wrote(h, req) {
		t.Error("asked for a ping the session already had")
	}
}

// With --json, TX handoffs stay hidden as in text output.
func TestJSONHidesHandoffs(t *testing.T) {
	var out bytes.Buffer
	p := &printer{json: true, out: &out}
	p.message(protocol.Handoff{ToApp: true})
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

// In half-duplex mode, a settle timeout after a write that went through
// means "sent", as in legacy mode; it used to fail the command.
func TestHalfDuplexSettleTimeoutIsNotFailure(t *testing.T) {
	h := newHarness()
	h.g.halfDuplex = true
	q := &halfDuplexTotem{subs: map[protocol.Channel]func([]byte){}, done: make(chan struct{})}
	h.g.connect = func(_ context.Context, opts client.Options) (*client.Client, error) {
		return client.New(q, opts)
	}
	s := h.session(t)
	if err := s.settle(); err != nil {
		t.Errorf("settle = %v", err)
	}
}

// `poi delete` sends a frame that deletes any peer: it must refuse a
// bonded Totem, which only re-bonding in person restores.
func TestPOIDeleteOnlyDeletesPOIs(t *testing.T) {
	h := newHarness()
	withPeers(h)
	err := h.run(t, "poi", "delete", peerA.String())
	if err == nil || !strings.Contains(err.Error(), "not a point of interest") {
		t.Errorf("deleting a bonded Totem: %v", err)
	}
	if _, _, peers, _ := h.state(); len(peers) != 2 {
		t.Fatalf("a bonded Totem was deleted: %v", peers)
	}

	h = newHarness()
	h.totem.Peers = []protocol.PeerPing{{MAC: protocol.MAC{2, 0xaa}, Name: "Stage", POI: true, RSSI: 100}}
	h.mustRun(t, "poi", "delete", "02aa00000000")
	if _, _, peers, _ := h.state(); len(peers) != 0 {
		t.Errorf("POI not deleted: %v", peers)
	}
}

// Every JSON line carries a type; scan used an anonymous struct, whose
// type name is empty.
func TestScanJSONType(t *testing.T) {
	var out bytes.Buffer
	p := &printer{json: true, out: &out}
	p.emit(Device{Name: "totem", Address: "8C:94:DF:7B:04:78", RSSI: -50})
	if !strings.HasPrefix(out.String(), `{"type":"Device","data":{"name":"totem"`) {
		t.Errorf("scan JSON line: %s", out.String())
	}
}
