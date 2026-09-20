package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial/enumerator"

	"github.com/ljagiello/totem-compass/mesh"
)

// capturedStatus is a status frame an LCFs Totem on 5.0.3 sent the emulator.
const capturedStatus = "a7740000d6581642bb03f4c2010079000002000000000000000000236786040df9ae6a000500030e00e604a243ffff00000000000000000000ff0000000000000000000000000a4c43467320746f74656d010153013401ff0000000000000000000000000000000000000000"

// fakeEmulator plays cmd/totememu's serial console: it answers the commands
// the mesh commands send with the JSON lines the firmware logs.
type fakeEmulator struct {
	mu   sync.Mutex
	cmds []string
	// reply returns the lines to answer a command with; nil means the
	// firmware's default answer.
	reply func(cmd string) []string
	// drop ignores this many "format json" commands, as if their bytes were
	// lost while the port opened.
	drop int
}

func (f *fakeEmulator) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	json := false
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		cmd := strings.TrimRight(sc.Text(), "\r")
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		drop := cmd == "format json" && f.drop > 0
		if drop {
			f.drop--
		}
		f.mu.Unlock()
		if cmd == "" || drop {
			continue
		}
		var lines []string
		if f.reply != nil {
			lines = f.reply(cmd)
		}
		if lines == nil {
			lines = defaultReply(cmd, &json)
		}
		for _, l := range lines {
			if _, err := io.WriteString(conn, l+"\r\n"); err != nil {
				return
			}
		}
	}
}

func defaultReply(cmd string, json *bool) []string {
	switch cmd {
	case "format json":
		*json = true
		return []string{`{"up":1000,"level":"INFO","msg":"log format","format":"json"}`}
	case "format text":
		*json = false
		return []string{`up=1s level=INFO msg="log format" format=text`}
	case "help":
		return []string{`{"up":1000,"level":"INFO","msg":"commands: pair | status"}`}
	case "log debug", "log info":
		return []string{fmt.Sprintf(`{"up":1000,"level":"INFO","msg":"log level","set":%q}`, strings.ToUpper(cmd[4:]))}
	}
	if !*json {
		return []string{"up=1s level=WARN msg=\"unknown command\""}
	}
	return []string{fmt.Sprintf(`{"up":1000,"level":"WARN","msg":"unknown command","line":%q}`, cmd)}
}

func (f *fakeEmulator) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

// waitFor waits for the commands to end with want; the last of them is sent
// as the session closes, so it can land after the command returns.
func (f *fakeEmulator) waitFor(t *testing.T, want []string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; {
		got := f.commands()
		if equalStrings(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("commands = %q, want %q", got, want)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// meshHarness wires a harness to a fake emulator on the only ESP32 port.
func meshHarness(f *fakeEmulator) *harness {
	h := newHarness()
	h.g.ports = func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{{Name: "/dev/cu.usbserial-0001", IsUSB: true, VID: "10c4", PID: "ea60"}}, nil
	}
	h.g.openPort = func(name string) (io.ReadWriteCloser, error) {
		if name != "/dev/cu.usbserial-0001" {
			return nil, fmt.Errorf("no port %s", name)
		}
		a, b := net.Pipe()
		go f.serve(b)
		return a, nil
	}
	return h
}

func TestMeshStatus(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if cmd != "status" {
			return nil
		}
		return []string{
			`{"up":1,"level":"INFO","msg":"self","mac":"209ba970abb0","name":"emu_totem_abb0","pairing":false,"sos":false,"heading":0,"color":0}`,
			`{"up":1,"level":"DEBUG","msg":"rx","src":"8c94df7b0478","dst":"209ba970abb0","rssi":-13,"len":108,"frame":"` + capturedStatus + `"}`,
			`{"up":1,"level":"INFO","msg":"peer","mac":"8c94df7b0478","name":"LCFs totem","rssi":-13,"heard_ms":700,"mesh":false,"lat":37.586754,"lon":-122.007301,"distance_m":-1,"batt":52}`,
		}
	}}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "status")
	for _, want := range []string{
		`emulator 209ba970abb0 "emu_totem_abb0"  pairing off  sos off  heading 0°  no fix`,
		`peer     8c94df7b0478 "LCFs totem"  rssi -13  heard 700ms ago  37.586754,-122.007301  batt 52%`,
	} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, h.out.String())
		}
	}
	f.waitFor(t, []string{"", "format json", "status", "format text"})
}

func TestMeshStatusNoPeers(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if cmd == "status" {
			return []string{`{"up":1,"level":"INFO","msg":"self","mac":"209ba970abb0","name":"emu_totem_abb0","pairing":true,"sos":true,"heading":90,"color":0,"lat":37.5,"lon":-122,"acc":3}`}
		}
		return nil
	}}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "status")
	for _, want := range []string{"pairing on  sos on  heading 90°  37.500000,-122.000000", "no peers: run totemctl mesh pair"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, h.out.String())
		}
	}
}

func watchReply(cmd string) []string {
	if cmd != "log debug" {
		return nil
	}
	return []string{
		`{"up":1000,"level":"INFO","msg":"log level","set":"DEBUG"}`,
		`{"up":1,"level":"DEBUG","msg":"rx","src":"8c94df7b0478","dst":"209ba970abb0","rssi":-13,"len":108,"frame":"` + capturedStatus + `"}`,
		`{"up":1,"level":"INFO","msg":"peer status","mac":"8c94df7b0478"}`,
		`{"up":1,"level":"DEBUG","msg":"tx","dst":"8c94df7b0478","len":108,"frame":"` + capturedStatus + `"}`,
		`{"up":1,"level":"INFO","msg":"bonded","mac":"8c94df7b0478","name":"LCFs totem","peers":1}`,
		`{"up":1,"level":"DEBUG","msg":"rx","src":"8c94df7b0478","dst":"ffffffffffff","rssi":-40,"len":3,"frame":"a774"}`,
		`[wifi][ESPNOW] I (2080) ESPNOW: espnow [version: 2.0] init`,
	}
}

func TestMeshWatch(t *testing.T) {
	f := &fakeEmulator{reply: watchReply}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "watch", "--for", "300ms")
	out := h.out.String()
	for _, want := range []string{
		`← 8c94df7b0478  -13 dBm  status "LCFs totem"  37.586754,-122.007286 ±1m alt 14m  azimuth 121° flat  batt 52% 3.82V  fw 5.0.3  up 20h54m0s  clock `,
		`· bonded mac=8c94df7b0478 name="LCFs totem" peers=1`,
		`← 8c94df7b0478  -40 dBm  invalid frame: mesh: not a Totem frame`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"→", "peer status", "log level", "ESPNOW"} {
		if strings.Contains(out, hidden) {
			t.Errorf("output shows %q:\n%s", hidden, out)
		}
	}
	f.waitFor(t, []string{"", "format json", "log debug", "log info", "format text"})
}

// TestMeshWatchSurvivesLineNoise: a burst without newlines, such as the
// bytes a USB bridge holds while nobody reads the port, must not end the
// session. bufio.Scanner used to give up for good on it.
func TestMeshWatchSurvivesLineNoise(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if cmd != "log debug" {
			return nil
		}
		return []string{
			`{"up":1000,"level":"INFO","msg":"log level","set":"DEBUG"}`,
			strings.Repeat("x", 100_000),
			`{"up":1,"level":"DEBUG","msg":"rx","src":"8c94df7b0478","dst":"209ba970abb0","rssi":-13,"len":108,"frame":"` + capturedStatus + `"}`,
		}
	}}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "watch", "--for", "500ms")
	if !strings.Contains(h.out.String(), `status "LCFs totem"`) {
		t.Errorf("no frame after the noise:\n%s", h.out.String())
	}
}

func TestMeshWatchTXAndRaw(t *testing.T) {
	h := meshHarness(&fakeEmulator{reply: watchReply})
	h.mustRun(t, "mesh", "watch", "--for", "300ms", "--tx", "--raw")
	out := h.out.String()
	if !strings.Contains(out, `→ 8c94df7b0478           status "LCFs totem"`) {
		t.Errorf("--tx does not show sent frames:\n%s", out)
	}
	if !strings.Contains(out, "\n         "+capturedStatus+"\n") {
		t.Errorf("--raw does not print the bytes:\n%s", out)
	}
}

func TestMeshWatchJSON(t *testing.T) {
	h := meshHarness(&fakeEmulator{reply: watchReply})
	h.mustRun(t, "mesh", "watch", "--for", "300ms", "--json")
	var frame struct {
		Type string `json:"type"`
		Data struct {
			Dir, Kind, Raw string
			Src, Dst       string
			RSSI           int
			Frame          mesh.Peer
		} `json:"data"`
	}
	first, _, _ := strings.Cut(h.out.String(), "\n")
	if err := json.Unmarshal([]byte(first), &frame); err != nil {
		t.Fatalf("%v: %s", err, first)
	}
	d := frame.Data
	if frame.Type != "MeshFrame" || d.Dir != "rx" || d.Kind != "Peer" || d.Src != "8c94df7b0478" || d.RSSI != -13 ||
		d.Frame.Name != "LCFs totem" || d.Frame.ReleaseID != 339 || d.Raw != capturedStatus {
		t.Errorf("first JSON line = %+v", frame)
	}
	if !strings.Contains(h.out.String(), `{"type":"MeshEvent","data":{"level":"INFO","msg":"bonded","attrs":{"mac":"8c94df7b0478","name":"LCFs totem","peers":1}}}`) {
		t.Errorf("no bonded event:\n%s", h.out.String())
	}
}

func TestMeshPair(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if cmd != "format json" {
			return nil
		}
		return []string{
			`{"up":1000,"level":"INFO","msg":"log format","format":"json"}`,
			`{"up":1,"level":"INFO","msg":"owned Totem is pairing but too far away","mac":"8c94df7b0478","rssi":-40,"need":-25}`,
			`{"up":1,"level":"INFO","msg":"register sender as peer to bond to","mac":"8c94df7b0478","rssi":-4}`,
			`{"up":1,"level":"INFO","msg":"bonded","mac":"8c94df7b0478","name":"LCFs totem","peers":1}`,
		}
	}}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "pair")
	for _, want := range []string{"too far away", "register sender", "bonded"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, h.out.String())
		}
	}
}

func TestMeshPairTimesOut(t *testing.T) {
	h := meshHarness(&fakeEmulator{})
	err := h.run(t, "mesh", "pair", "--for", "200ms")
	if err == nil || !strings.Contains(err.Error(), "no bond within 200ms") {
		t.Fatalf("err = %v, want a timeout that explains the gesture", err)
	}
}

func TestMeshSend(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if cmd == "pos 37.5 -122 3" {
			return []string{`{"up":1,"level":"INFO","msg":"position set","lat":37.5,"lon":-122,"acc":3}`}
		}
		return nil
	}}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "send", "pos", "37.5", "-122", "3")
	if !strings.Contains(h.out.String(), "· position set acc=3 lat=37.5 lon=-122") {
		t.Errorf("output = %q", h.out.String())
	}
	err := h.run(t, "mesh", "send", "fly")
	if err == nil || !strings.Contains(err.Error(), `does not know "fly"`) {
		t.Errorf("unknown command: err = %v", err)
	}
}

func TestMeshClock(t *testing.T) {
	var got string
	f := &fakeEmulator{reply: func(cmd string) []string {
		if !strings.HasPrefix(cmd, "clock ") {
			return nil
		}
		got = cmd
		return []string{`{"up":1,"level":"INFO","msg":"clock set","wall":"2026-09-19T22:30:00Z"}`}
	}}
	h := meshHarness(f)
	h.mustRun(t, "mesh", "clock")
	ms, err := strconv.ParseInt(strings.TrimPrefix(got, "clock "), 10, 64)
	if err != nil {
		t.Fatalf("sent %q", got)
	}
	if d := time.Since(time.UnixMilli(ms)).Abs(); d > time.Minute {
		t.Errorf("sent a clock %v off this computer's", d)
	}
	if !strings.Contains(h.out.String(), "clock set") {
		t.Errorf("output = %q", h.out.String())
	}
}

func TestMeshClockRejected(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if strings.HasPrefix(cmd, "clock ") {
			return []string{`{"up":1,"level":"WARN","msg":"bad command","err":"clock 1970-01-01T00:00:07Z is before 2020"}`}
		}
		return nil
	}}
	h := meshHarness(f)
	if err := h.run(t, "mesh", "clock"); err == nil || !strings.Contains(err.Error(), "rejected the clock") {
		t.Errorf("err = %v", err)
	}
}

func TestMeshSendRejected(t *testing.T) {
	f := &fakeEmulator{reply: func(cmd string) []string {
		if cmd == "pos NaN 0" {
			return []string{`{"up":1,"level":"WARN","msg":"bad command","err":"pos: latitude: \"NaN\": want -90 to 90 degrees"}`}
		}
		return nil
	}}
	h := meshHarness(f)
	err := h.run(t, "mesh", "send", "pos", "NaN", "0")
	if err == nil || !strings.Contains(err.Error(), `rejected "pos NaN 0": pos: latitude`) {
		t.Errorf("err = %v, want the emulator's reason", err)
	}
}

func TestMeshHandshakeRetries(t *testing.T) {
	f := &fakeEmulator{drop: 1}
	h := meshHarness(f)
	h.g.timeScale = 1 // one retry needs a real second
	h.mustRun(t, "mesh", "send", "help")
	if n := strings.Count(strings.Join(f.commands(), "\n"), "format json"); n != 2 {
		t.Errorf("sent format json %d times, want 2 (one lost, one answered): %q", n, f.commands())
	}
}

func TestMeshNoEmulator(t *testing.T) {
	h := meshHarness(&fakeEmulator{drop: 1 << 30})
	err := h.run(t, "mesh", "status")
	if !errors.Is(err, errNoEmulator) || !strings.Contains(err.Error(), "/dev/cu.usbserial-0001") {
		t.Fatalf("err = %v, want errNoEmulator naming the port", err)
	}
}

func TestEmulatorPort(t *testing.T) {
	cp2102 := &enumerator.PortDetails{Name: "/dev/cu.usbserial-0001", IsUSB: true, VID: "10C4"}
	twin := &enumerator.PortDetails{Name: "/dev/tty.usbserial-0001", IsUSB: true, VID: "10C4"}
	ch340 := &enumerator.PortDetails{Name: "/dev/ttyUSB0", IsUSB: true, VID: "1a86"}
	modem := &enumerator.PortDetails{Name: "/dev/cu.Bluetooth-Incoming-Port"}
	tests := []struct {
		name  string
		flag  string
		ports []*enumerator.PortDetails
		err   error
		want  string
	}{
		{"flag wins", "/dev/ttyACM3", []*enumerator.PortDetails{cp2102}, nil, "/dev/ttyACM3"},
		{"one board, macOS twins", "", []*enumerator.PortDetails{modem, twin, cp2102}, nil, "/dev/cu.usbserial-0001"},
		{"lowercase vid", "", []*enumerator.PortDetails{ch340}, nil, "/dev/ttyUSB0"},
		{"none", "", []*enumerator.PortDetails{modem}, nil, "no ESP32 USB serial port found"},
		{"several", "", []*enumerator.PortDetails{cp2102, ch340}, nil, "several ESP32 serial ports (/dev/cu.usbserial-0001, /dev/ttyUSB0)"},
		{"list fails", "", nil, errors.New("boom"), "list serial ports: boom (pass --port)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &globals{port: tt.flag, ports: func() ([]*enumerator.PortDetails, error) { return tt.ports, tt.err }}
			got, err := g.emulatorPort()
			if err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("emulatorPort() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMeshPortFromEnvironment(t *testing.T) {
	t.Setenv("TOTEM_PORT", "/dev/cu.usbserial-0001")
	h := meshHarness(&fakeEmulator{})
	h.g.ports = func() ([]*enumerator.PortDetails, error) { return nil, errors.New("must not list ports") }
	h.mustRun(t, "mesh", "send", "help")
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00") && len(a) == len(b)
}

// FuzzConsoleLine feeds arbitrary console output to the line parser and the
// frame formatter: whatever a board prints must not crash the CLI.
func FuzzConsoleLine(f *testing.F) {
	for _, l := range watchReply("log debug") {
		f.Add([]byte(l))
	}
	f.Add([]byte(`{"level":"DEBUG","msg":"rx","frame":"a77407000000"}`))
	f.Add([]byte(`{"level":"INFO","msg":"self","lat":"x"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		l, err := parseConsoleLine(b)
		if err != nil {
			return
		}
		if l.Msg == "" || l.Level == "" {
			t.Fatalf("accepted a line without msg or level: %q", b)
		}
		for _, k := range []string{"up", "level", "msg"} {
			if _, ok := l.attrs[k]; ok {
				t.Fatalf("attrs keep the built-in key %q", k)
			}
		}
		fr := decodeFrame(l)
		if (fr.Frame == nil) != (fr.Kind == "invalid") {
			t.Fatalf("frame %v with kind %q", fr.Frame, fr.Kind)
		}
		if frameLine(fr) == "" {
			t.Fatal("empty frame line")
		}
		var out strings.Builder
		p := &printer{out: &out}
		p.meshEvent(l.event())
		p.meshFrame(fr, true)
		p.meshStatus(l, []consoleLine{l})
	})
}

// TestMeshStatusNeverHeard: a peer restored from the bond list reports
// heard_ms -1, and a board that has just booted can report an age no
// duration holds. Both read as "never", rather than as an overflowed
// negative age.
func TestMeshStatusNeverHeard(t *testing.T) {
	for _, ms := range []string{"-1", "1700000000000000000"} {
		var out strings.Builder
		p := &printer{out: &out}
		self := mustLine(t, `{"up":1,"level":"INFO","msg":"self","mac":"209ba970abb0","name":"emu_totem_abb0","pairing":false,"sos":false,"heading":0,"color":0}`)
		peer := mustLine(t, `{"up":1,"level":"INFO","msg":"peer","mac":"8c94df7b0478","name":"LCFs totem","rssi":-13,"heard_ms":`+ms+
			`,"mesh":false,"lat":37.586754,"lon":-122.007301,"distance_m":-1,"batt":52}`)
		p.meshStatus(self, []consoleLine{peer})
		if got := out.String(); !strings.Contains(got, "heard never") {
			t.Errorf("heard_ms %s printed:\n%s", ms, got)
		}
	}
}

func mustLine(t *testing.T, s string) consoleLine {
	t.Helper()
	l, err := parseConsoleLine([]byte(s))
	if err != nil {
		t.Fatalf("parseConsoleLine(%q): %v", s, err)
	}
	return l
}
