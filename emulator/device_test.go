package emulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCrystalHoldStartsPairing: the gesture a person uses to pair is a
// hold of the Touch Crystal for about 1.2 s. A shorter press is a tap and
// must not open the radio to a bond.
func TestCrystalHoldStartsPairing(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce) // touch is disabled for a moment after boot

	h.n.Tap(Crystal, 1, h.now)
	h.advance(multiTapWindow + time.Second)
	if h.n.Pairing() {
		t.Fatal("a tap of the crystal started pairing")
	}
	h.collect(h.n.HoldFor(Crystal, crystalPairHold+100*time.Millisecond, h.now))
	if !h.n.Pairing() {
		t.Fatal("holding the crystal did not start pairing")
	}
	if got := h.n.LEDs().Animation(); got != AnimPairing {
		t.Errorf("animation while pairing = %s", got)
	}
	h.advance(pairingWindow + time.Second)
	if h.n.Pairing() {
		t.Error("pairing did not end with its window")
	}
	if got := h.n.LEDs().Animation(); got == AnimPairing {
		t.Error("the pairing animation is still playing")
	}
}

// TestTouchIsDeadAfterBoot: the firmware waits before enabling touch
// ("Booted: {} ago, waiting: {} before enabling touch"), so a device
// being picked up does not pair itself.
func TestTouchIsDeadAfterBoot(t *testing.T) {
	h := newHarness(t, nil)
	h.collect(h.n.HoldFor(Crystal, 2*time.Second, h.now))
	if h.n.Pairing() {
		t.Fatal("the crystal worked before the post-boot wait was over")
	}
	h.advance(bootDebounce)
	h.collect(h.n.HoldFor(Crystal, 2*time.Second, h.now))
	if !h.n.Pairing() {
		t.Fatal("the crystal did not work after the wait")
	}
}

// TestButtonGestures: the two physical buttons carry the wiring compass
// gives them. A tap of power toggles brightness, a hold of SOS starts the
// alarm, and a tap of SOS mutes it without turning it off.
func TestButtonGestures(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)

	full := h.n.LEDs().Brightness()
	h.n.Tap(PowerButton, 1, h.now)
	h.advance(multiTapWindow + 200*time.Millisecond)
	if dim := h.n.LEDs().Brightness(); dim >= full {
		t.Errorf("a tap of power left brightness at %v", dim)
	}
	h.n.Tap(PowerButton, 1, h.now)
	h.advance(multiTapWindow + 200*time.Millisecond)
	if got := h.n.LEDs().Brightness(); got != full {
		t.Errorf("a second tap left brightness at %v, want %v", got, full)
	}

	h.collect(h.n.HoldFor(SOSButton, holdTime+100*time.Millisecond, h.now))
	// The hold ran ahead of the harness clock; catch up, or the next
	// press looks like an edge from the past and is ignored.
	h.advance(holdTime + 200*time.Millisecond)
	if !h.n.Config().SOS {
		t.Fatal("holding the SOS button did not start the alarm")
	}
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Errorf("animation with SOS on = %s", got)
	}
	h.n.Tap(SOSButton, 1, h.now)
	h.advance(multiTapWindow + 200*time.Millisecond)
	if !h.n.Config().SOS {
		t.Error("muting turned the alarm off; it should only stop the blinking")
	}
	if got := h.n.LEDs().Animation(); got == AnimSOS {
		t.Error("the crystal is still blinking after a mute")
	}
}

// TestPowerButtonHoldPowersDown: a held power button is device_off. The
// radio stops, which is what a peer sees.
func TestPowerButtonHoldPowersDown(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.advance(bootDebounce)
	h.take()

	h.collect(h.n.HoldFor(PowerButton, holdTime+100*time.Millisecond, h.now))
	if !h.n.Power().Off() {
		t.Fatal("holding power did not power the device down")
	}
	h.advance(30 * time.Second)
	if s := h.take(); len(s) != 0 {
		t.Errorf("a powered-down device sent %d frames", len(s))
	}
	h.n.PowerOn(h.now)
	h.advance(30 * time.Second)
	if len(h.take()) == 0 {
		t.Error("the device sent nothing after being powered back on")
	}
}

// TestBatteryDrivesThePowerMode: the mode a Totem reports follows the
// battery, down to the cutoff where it powers itself off rather than
// brown out.
func TestBatteryDrivesThePowerMode(t *testing.T) {
	tests := []struct {
		volts float32
		pct   int8
		want  PowerMode
	}{
		{4.10, 95, PowerNormal},
		{3.75, 35, PowerEco},
		{3.44, 8, PowerLow},
		{3.29, 1, PowerOff},
	}
	for _, tt := range tests {
		h := newHarness(t, nil)
		if err := h.n.SetBattery(tt.pct, false, h.now); err != nil {
			t.Fatal(err)
		}
		// SetBattery maps the level to a voltage; force the one under test.
		h.n.sensors.Battery.Volts = tt.volts
		h.n.sensors.Battery.Low = tt.volts <= lowBattVolts
		h.n.device(h.now)
		if got := h.n.Power().Mode(); got != tt.want {
			t.Errorf("%.2f V at %d%% gave %s, want %s", tt.volts, tt.pct, got, tt.want)
		}
	}
}

// TestBatteryCurve: v5.0.3 recalibrated get_batt_pct so a normal full
// charge reads 100%. The three points the disassembly gives have to hold.
func TestBatteryCurve(t *testing.T) {
	tests := []struct {
		volts float32
		want  int8
	}{
		{4.20, 100}, {4.12, 100}, {4.00, 86}, {3.80, 50}, {3.30, 0}, {3.00, 0},
	}
	for _, tt := range tests {
		if got := battPctFor(tt.volts); got != tt.want {
			t.Errorf("battPctFor(%.2f) = %d, want %d", tt.volts, got, tt.want)
		}
	}
	// And it never goes backwards as the voltage rises.
	last := int8(-1)
	for v := float32(3.0); v <= 4.3; v += 0.01 {
		got := battPctFor(v)
		if got < last {
			t.Fatalf("the curve dips at %.2f V: %d after %d", v, got, last)
		}
		last = got
	}
}

// TestWatchdogBlockers: wdt_manager feeds the watchdog only while nothing
// is blocking, and a flash write is one of the blockers.
func TestWatchdogBlockers(t *testing.T) {
	p := newPower(t0)
	if !p.WatchdogFeeding() {
		t.Fatal("a fresh device is not feeding the watchdog")
	}
	p.Block(BlockVFSWrite)
	if p.WatchdogFeeding() {
		t.Error("the watchdog is still fed during a write")
	}
	p.Block(BlockWLANKick)
	p.Unblock(BlockVFSWrite)
	if p.WatchdogFeeding() {
		t.Error("one blocker released is not all of them")
	}
	p.Unblock(BlockWLANKick)
	if !p.WatchdogFeeding() {
		t.Error("the feed loop did not come back")
	}
}

// TestSleepStopsShortOfAWindow: a Totem does not sleep into a radio
// window ("Not sleeping due radio needing to turn on soon"), and it does
// not sleep at all while something holds it awake.
func TestSleepStopsShortOfAWindow(t *testing.T) {
	p := newPower(t0)
	if d := p.sleep(t0, t0.Add(time.Second)); d != time.Second-wakeBeforeWindow {
		t.Errorf("slept %v before a window a second away", d)
	}
	if d := p.sleep(t0, t0.Add(wakeBeforeWindow/2)); d != 0 {
		t.Errorf("slept %v into a window that was about to open", d)
	}
	p.HoldSleep(true)
	if d := p.sleep(t0, t0.Add(time.Minute)); d != 0 {
		t.Errorf("slept %v while sleep was held off", d)
	}
	p.HoldSleep(false)
	if p.Duty() <= 0 || p.Duty() > 1 {
		t.Errorf("duty = %v", p.Duty())
	}
}

// TestColorsAreTheFirmwarePalette: the colour ids in frames index this
// table, so the names and values have to be the firmware's.
func TestColorsAreTheFirmwarePalette(t *testing.T) {
	if got := len(palette); got != 13 {
		t.Fatalf("the palette holds %d colors, want 13", got)
	}
	for _, tt := range []struct {
		c    Color
		name string
		rgb  RGB
	}{
		{ColorRed, "red", RGB{255, 0, 0}},
		{ColorTeal, "teal", RGB{0, 255, 255}},
		{ColorAqua, "aqua", RGB{0, 128, 255}},
		{ColorHotPink, "hot_pink", RGB{255, 0, 128}},
	} {
		if got := tt.c.String(); got != tt.name {
			t.Errorf("%d is named %q, want %q", int(tt.c), got, tt.name)
		}
		if got := tt.c.RGB(); got != tt.rgb {
			t.Errorf("%s = %v, want %v", tt.name, got, tt.rgb)
		}
		back, err := ParseColor(tt.name)
		if err != nil || back != tt.c {
			t.Errorf("ParseColor(%q) = %v, %v", tt.name, back, err)
		}
	}
	// A colour id from a frame can be anything; it must not index out.
	for _, c := range []Color{-1, -128, 13, 127} {
		if got := c.RGB(); got != Off {
			t.Errorf("colour %d rendered as %v", int(c), got)
		}
		if s := c.String(); s == "" {
			t.Errorf("colour %d has no name", int(c))
		}
	}
}

// TestDialPointsAtThePeer: at rest the ring shows where the bonded Totem
// is, which is what the compass is for.
func TestDialPointsAtThePeer(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	// A status frame from due north of us.
	north := statusFrame(t0)
	north.Lat, north.Lon = 37.785, -122.42
	h.rx(totem, self, -40, north)
	h.advance(5 * time.Second)

	desc := h.n.LEDs().Describe()
	if !strings.Contains(desc, "pointing") {
		t.Fatalf("the ring is not pointing at the peer: %s", desc)
	}
	lit := 0
	for _, p := range h.n.LEDs().Ring() {
		if p != Off {
			lit++
		}
	}
	if lit == 0 || lit > 5 {
		t.Errorf("%d pixels lit for a dial, want a point and its neighbors", lit)
	}
}

// fakeOTA is an update server: it answers the three exchanges a Totem
// makes, and remembers what it was asked.
type fakeOTA struct {
	release  string
	contents string
	size     int64
	fail     error
	posts    []string
	gets     []string
}

func (f *fakeOTA) Post(url string, body []byte) ([]byte, error) {
	f.posts = append(f.posts, url)
	if f.fail != nil {
		return nil, f.fail
	}
	var poll releasePoll
	if err := json.Unmarshal(body, &poll); err != nil {
		return nil, fmt.Errorf("the device sent a body that is not JSON: %w", err)
	}
	if poll.Version == "" || poll.DeviceTypeID == 0 {
		return nil, fmt.Errorf("the device did not say what it is: %+v", poll)
	}
	return []byte(f.release), nil
}

func (f *fakeOTA) Get(url string) ([]byte, error) {
	f.gets = append(f.gets, url)
	if f.fail != nil {
		return nil, f.fail
	}
	return []byte(f.contents), nil
}

func (f *fakeOTA) Download(url string, progress func(done, total int64)) (int64, string, error) {
	f.gets = append(f.gets, url)
	if f.fail != nil {
		return 0, "", f.fail
	}
	for done := int64(0); done < f.size; done += f.size / 4 {
		progress(done, f.size)
	}
	progress(f.size, f.size)
	return f.size, "0000000000000000000000000000000000000000000000000000000000000000", nil
}

func newFakeOTA() *fakeOTA {
	return &fakeOTA{
		release: `{"ota_url":"http://ota.example/repo","version":"5.0.4","product":"totem_compass",
			"branch":"totem","release_code":"5.0.4","release_id":340}`,
		contents: `["firmware_v5.0.4.bin","notes.txt"]`,
		size:     1 << 20,
	}
}

// TestUpdateRunsTheExchange: the update asks the API what release it
// should be on, fetches the index from the URL it answers, picks the .bin
// and downloads it, showing progress on the ring.
func TestUpdateRunsTheExchange(t *testing.T) {
	srv := newFakeOTA()
	h := newHarness(t, func(c *Config) { c.OTATransport = srv })
	if err := h.n.SetBattery(80, false, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.n.Update(h.now); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := h.n.OTA().State(); got != OTADone {
		t.Errorf("state = %s, want done", got)
	}
	if got := h.n.OTA().Package(); got != "firmware_v5.0.4.bin" {
		t.Errorf("picked %q", got)
	}
	if len(srv.posts) != 1 || !strings.HasSuffix(srv.posts[0], "/ota") {
		t.Errorf("posts = %v", srv.posts)
	}
	if !strings.HasPrefix(srv.posts[0], OTAEndpoint+"/devices/209BA970ABB0/") {
		t.Errorf("the device did not ask about itself in upper-case hex: %v", srv.posts[0])
	}
	if len(srv.gets) != 2 || !strings.HasSuffix(srv.gets[0], "/contents.json") ||
		!strings.HasSuffix(srv.gets[1], "firmware_v5.0.4.bin") {
		t.Errorf("gets = %v", srv.gets)
	}
	if p := h.n.OTA().Progress(); p != 1 {
		t.Errorf("progress ended at %v", p)
	}
}

// TestUpdateRefusesOnAFlatBattery: "Battery too low for OTA update". An
// update reboots the device, and a device that reboots flat does not come
// back.
func TestUpdateRefusesOnAFlatBattery(t *testing.T) {
	srv := newFakeOTA()
	h := newHarness(t, func(c *Config) { c.OTATransport = srv })
	if err := h.n.SetBattery(10, false, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Fatalf("update on a flat battery: %v", err)
	}
	if len(srv.posts) != 0 {
		t.Error("it asked the server before checking the battery")
	}
	// Charging is different: the device is on power.
	if err := h.n.SetBattery(10, true, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.n.Update(h.now); err != nil {
		t.Errorf("update while charging: %v", err)
	}
}

// TestUpdateWithoutANetwork: a board with no transport says so.
func TestUpdateWithoutANetwork(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.Update(h.now); !errors.Is(err, ErrNoTransport) {
		t.Fatalf("update with no transport: %v", err)
	}
	if got := h.n.OTA().State(); got != OTAFailed {
		t.Errorf("state = %s", got)
	}
}

// TestUpdateRejectsWhatTheServerSays: the OTA exchange is plain HTTP with
// no key and no signature, so everything in it is checked here.
func TestUpdateRejectsWhatTheServerSays(t *testing.T) {
	tests := map[string]struct {
		release, contents string
		want              error
	}{
		"no ota_url":      {`{"version":"5.0.4"}`, `["firmware_v5.0.4.bin"]`, ErrBadRelease},
		"not json":        {`<html>404</html>`, `["firmware_v5.0.4.bin"]`, ErrBadRelease},
		"url not http":    {`{"ota_url":"file:///etc/passwd"}`, `["a_v1.bin"]`, ErrBadRelease},
		"index not json":  {`{"ota_url":"http://ota.example/repo"}`, `not json`, ErrBadContents},
		"index has paths": {`{"ota_url":"http://ota.example/repo"}`, `["../../etc/passwd"]`, ErrBadContents},
		"no package":      {`{"ota_url":"http://ota.example/repo"}`, `["notes.txt"]`, ErrNoPackage},
	}
	for name, tt := range tests {
		srv := newFakeOTA()
		srv.release, srv.contents = tt.release, tt.contents
		h := newHarness(t, func(c *Config) { c.OTATransport = srv })
		err := h.n.Update(h.now)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
		if got := h.n.OTA().State(); got != OTAFailed {
			t.Errorf("%s: state = %s", name, got)
		}
	}
}

func TestVersionFromName(t *testing.T) {
	for name, want := range map[string]string{
		"firmware_v5.0.3.bin": "5.0.3",
		"preview_v12.tgz":     "12",
		"a_b_v1.2.3.bin":      "1.2.3",
	} {
		got, err := versionFromName(name)
		if err != nil || got != want {
			t.Errorf("versionFromName(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"firmware.bin", "_v.bin", "v5.0.3.bin", "firmware_v5.0.3"} {
		if got, err := versionFromName(name); err == nil {
			t.Errorf("versionFromName(%q) = %q, want an error", name, got)
		}
	}
}

// TestOTAReportKeys: a server on the other side of an update has to
// accept this body, so its field names are part of the contract.
func TestOTAReportKeys(t *testing.T) {
	srv := newFakeOTA()
	h := newHarness(t, func(c *Config) {
		c.OTATransport = srv
		c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3}
	})
	if err := h.n.SetBattery(90, false, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.n.Update(h.now); err != nil {
		t.Fatal(err)
	}
	b, err := h.n.OTAReport(7, h.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"boot_count", "branch", "device_age", "device_type_id",
		"product", "release_code", "release_id", "lat", "lon", "gnss_time"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the report has no %q: %s", key, b)
		}
	}
	if got["release_id"] != float64(340) || got["boot_count"] != float64(7) {
		t.Errorf("report = %s", b)
	}
}
