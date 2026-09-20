package emulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ljagiello/totem-compass/mesh"
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
	now := t0
	if d := p.sleep(now, now.Add(time.Second)); d != time.Second-wakeBeforeWindow {
		t.Errorf("reported %v of sleep before a window a second away", d)
	}
	now = now.Add(time.Second)
	if d := p.sleep(now, now.Add(wakeBeforeWindow/2)); d != 0 {
		t.Errorf("reported %v of sleep into a window that was about to open", d)
	}
	now = now.Add(time.Second)
	p.HoldSleep(true)
	if d := p.sleep(now, now.Add(time.Minute)); d != 0 {
		t.Errorf("reported %v of sleep while sleep was held off", d)
	}
	// Nothing here could be counted as slept: the only interval that
	// might have been is the first, and no time had passed by then. What
	// the duty cycle does over a run is the next test's job.
	if p.SleptMs() != 0 {
		t.Errorf("slept %d ms across three calls that could not sleep", p.SleptMs())
	}
}

// TestSleepCannotExceedTheClock: the counter is what the firmware logs as
// dev_total_lightsleep_ms, and since it is saved to flash it has to be a
// real number. Counting the sleep still ahead on every call multiplied it
// by however often the driver polled: ten seconds of node time reported
// hundreds of hours.
func TestSleepCannotExceedTheClock(t *testing.T) {
	p := newPower(t0)
	now := t0
	// A driver polling every 5 ms with the next event four seconds away.
	for i := 0; i < 2000; i++ {
		now = now.Add(5 * time.Millisecond)
		p.sleep(now, now.Add(4*time.Second))
	}
	elapsed := now.Sub(t0).Milliseconds()
	if p.SleptMs() > elapsed {
		t.Errorf("slept %d ms in %d ms of clock", p.SleptMs(), elapsed)
	}
	if p.SleptMs() < elapsed/2 {
		t.Errorf("slept only %d ms of an idle %d ms", p.SleptMs(), elapsed)
	}
	if d := p.Duty(); d <= 0 || d > 1 {
		t.Errorf("duty = %v", d)
	}
}

// TestChargingBeatsTheCutoff: a pack that reads flat while it charges is
// filling up. Powering down there would kill the radio at the moment
// someone plugged it in.
func TestChargingBeatsTheCutoff(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.n.SetBattery(0, true, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.device(h.now)
	if h.n.Power().Off() {
		t.Fatal("a charging device powered itself down")
	}
	if got := h.n.Power().Mode(); got != PowerNormal {
		t.Errorf("mode on the charger = %s", got)
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

// TestCutoffStopsTheRadio: "Voltages too low, powering down" has to stop
// the device, not just label it. A peer's Totem sees a flat device go
// quiet; one that kept transmitting would be reported as fine until it
// browned out.
func TestCutoffStopsTheRadio(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.advance(10 * time.Second)
	if len(h.take()) == 0 {
		t.Fatal("a healthy device sent nothing")
	}
	if err := h.n.SetBattery(1, false, h.now); err != nil {
		t.Fatal(err)
	}
	h.n.sensors.Battery.Volts = cutoffVolts - 0.01
	h.n.device(h.now)
	if !h.n.Power().Off() {
		t.Fatal("a device under the cutoff is still on")
	}
	h.advance(time.Minute)
	if s := h.take(); len(s) != 0 {
		t.Errorf("a device past its cutoff sent %d frames", len(s))
	}
}

// TestVoltsAndPercentAgree: a level set by hand and the voltage reported
// with it have to be two views of the same battery, or a peer sees a
// device at 50% holding a voltage the curve calls 41%.
func TestVoltsAndPercentAgree(t *testing.T) {
	for p := int8(0); p <= 100; p += 5 {
		v := voltsFor(p)
		if back := battPctFor(v); back < p-2 || back > p+2 {
			t.Errorf("%d%% is %.3f V, which reads back as %d%%", p, v, back)
		}
	}
}

// TestPoweredDownDeviceIsDeaf: a Totem that has powered down has its
// radio off. It does not answer a frame, and it does not take a bond,
// which is what someone standing next to it sees.
func TestPoweredDownDeviceIsDeaf(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.bond()
	h.take()
	h.n.PowerOff(h.now)

	h.rx(totem, self, -20, statusFrame(t0))
	h.advance(10 * time.Second)
	if s := h.take(); len(s) != 0 {
		t.Errorf("a powered-down device answered with %d frames", len(s))
	}
	// And it comes back when it is switched on again.
	h.n.PowerOn(h.now)
	h.rx(totem, self, -20, statusFrame(t0))
	h.advance(10 * time.Second)
	if len(h.take()) == 0 {
		t.Error("it stayed deaf after being powered back on")
	}
}

// TestIdleStripStopsAskingForFrames: with nothing to show, the LED model
// must not keep the device awake for a picture that does not change.
func TestIdleStripStopsAskingForFrames(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootAnim + time.Second) // past the power-up animation
	if got := h.n.LEDs().Animation(); got != AnimIdle {
		t.Fatalf("animation = %s, want idle", got)
	}
	if next := h.n.LEDs().Next(); !next.IsZero() {
		t.Errorf("an idle strip still wants a frame at %s", next)
	}
	// Something to point at brings the frames back.
	h.n.LEDs().SetDial(90, ColorTeal)
	if next := h.n.LEDs().Next(); next.IsZero() {
		t.Error("the strip did not wake up for the compass dial")
	}
}

// TestPoweredDownIgnoresEveryGestureButOne: a Totem that is off does not
// pair, does not raise an alarm and does not start an update. Holding
// the power button is the one thing that reaches it.
func TestPoweredDownIgnoresEveryGestureButOne(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	h.n.PowerOff(h.now)

	h.collect(h.n.HoldFor(Crystal, crystalPairHold+100*time.Millisecond, h.now))
	h.advance(time.Second)
	if h.n.Pairing() {
		t.Error("a powered-down device started pairing")
	}
	h.collect(h.n.HoldFor(SOSButton, holdTime+100*time.Millisecond, h.now))
	h.advance(time.Second)
	if h.n.Config().SOS {
		t.Error("a powered-down device raised the alarm")
	}
	if !h.n.Power().Off() {
		t.Fatal("it came back on by itself")
	}

	h.collect(h.n.HoldFor(PowerButton, holdTime+100*time.Millisecond, h.now))
	h.advance(time.Second)
	if h.n.Power().Off() {
		t.Error("holding the power button did not turn it back on")
	}
}

// TestRingSearchesForAFix: a Totem with no fix sweeps its ring while the
// receiver looks for one, and stops once it has one. It is the first
// thing someone sees after a boot indoors.
func TestRingSearchesForAFix(t *testing.T) {
	h := newHarness(t, nil) // no position
	// Past the power-up animation, and past the first radio window: the
	// search starts on the poll after the strip goes idle.
	h.advance(bootAnim + 10*time.Second)
	if got := h.n.LEDs().Animation(); got != AnimGNSSSearch {
		t.Fatalf("animation without a fix = %s, want the search", got)
	}
	// The sweep moves: two frames apart the ring is not the same picture.
	first := append([]RGB(nil), h.n.LEDs().Ring()...)
	h.advance(10 * ledFrame)
	same := true
	for i, p := range h.n.LEDs().Ring() {
		if p != first[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("the search animation is not moving")
	}

	if err := h.n.SetPosition(&Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3}, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	if got := h.n.LEDs().Animation(); got == AnimGNSSSearch {
		t.Error("it is still searching with a fix in hand")
	}

	// An alarm outranks the search.
	if err := h.n.SetPosition(nil, h.now); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	h.n.SetSOS(true)
	h.advance(time.Second)
	if got := h.n.LEDs().Animation(); got != AnimSOS {
		t.Errorf("animation with SOS on and no fix = %s, want the alarm", got)
	}
}

// TestTapCountsArrive: a double tap has to arrive as a double tap. The
// synthetic taps were spaced 60 ms apart while each release was 31 ms in,
// so every press after the first landed 29 ms after the last edge and the
// 30 ms lockout swallowed it: a requested triple tap reached the node as
// a double, and the triple tap is what starts an update.
func TestTapCountsArrive(t *testing.T) {
	for count, want := range map[int]Gesture{1: SingleTap, 2: DoubleTap, 3: TripleTap} {
		h := newHarness(t, nil)
		h.advance(bootDebounce)
		h.n.Tap(SOSButton, count, h.now)
		r := h.n.input(SOSButton)
		if r.taps != count {
			t.Errorf("%d taps registered as %d", count, r.taps)
		}
		got := r.poll(h.now.Add(tapStep*time.Duration(count) + multiTapWindow + time.Second))
		if len(got) != 1 || got[0] != want {
			t.Errorf("%d taps gave %v, want %v", count, got, want)
		}
	}
}

// TestOneWindowPerNode: a sensor source that arrives with a clock takes
// the GNSS path in read(), which schedules the windows itself. Scheduling
// another on top left two jobs rescheduling each other, and every peer
// saw every status frame twice.
func TestOneWindowPerNode(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sensors = NewStatic(Sensors{
			Fix:         &Fix{Lat: 37.775, Lon: -122.42, AccuracyM: 3, Time: t0},
			Orientation: mesh.OrientationVertical,
		})
	})
	windows := 0
	for _, j := range h.n.jobs {
		if j.kind == jobWindow {
			windows++
		}
	}
	if windows != 1 {
		t.Fatalf("%d window jobs at boot, want 1", windows)
	}
	h.bond()
	h.take()
	h.advance(20 * time.Second)
	var status int
	for _, s := range h.take() {
		if p, ok := s.msg.(mesh.Peer); ok && p.Command == mesh.PeerStatus {
			status++
		}
	}
	// Four-second windows: about five in twenty seconds, not ten.
	if status > 7 {
		t.Errorf("%d status frames in 20 s, which is about twice the windows", status)
	}
}

// TestPowerOffEndsPairing: powering down cleared the job list, which took
// the timer that ends pairing with it. The device came back still
// pairing, and a node that is pairing holds back every status frame.
func TestPowerOffEndsPairing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Position = &Position{Lat: 37.775, Lon: -122.42, AccuracyM: 3} })
	h.collect(h.n.Pair(h.now))
	if !h.n.Pairing() {
		t.Fatal("pairing did not start")
	}
	h.n.PowerOff(h.now)
	if h.n.Pairing() {
		t.Fatal("a powered-down device is still pairing")
	}
	h.n.PowerOn(h.now)
	h.bond()
	h.take()
	h.advance(20 * time.Second)
	if len(h.take()) == 0 {
		t.Error("the device sent nothing after being powered back on")
	}
}

// TestEveryAnimationEnds: an animation that never ends pins the strip at
// a frame every 25 ms, which keeps the modeled device awake for a
// picture nobody is watching.
func TestEveryAnimationEnds(t *testing.T) {
	// The ones that run until something stops them, and what stops them.
	openEnded := map[Animation]bool{AnimIdle: true, AnimPairing: true, AnimSOS: true,
		AnimOTA: true, AnimWiFi: true, AnimGNSSSearch: true}
	for a := AnimIdle; a <= AnimLowBattery; a++ {
		if openEnded[a] {
			continue
		}
		l := newLEDs(t0, ColorTeal)
		l.Play(a, t0)
		l.Tick(t0.Add(time.Minute))
		if got := l.Animation(); got == a {
			t.Errorf("%s is still playing a minute later", a)
		}
		if next := l.Next(); !next.IsZero() && next.After(t0.Add(time.Minute+time.Second)) {
			t.Errorf("%s left the strip wanting a frame at %s", a, next)
		}
	}
}

// TestUpdateRefusesAtZeroPercent: 0% is the flattest reading there is, so
// it belongs inside the battery gate. It used to fall outside it.
func TestUpdateRefusesAtZeroPercent(t *testing.T) {
	srv := newFakeOTA()
	h := newHarness(t, func(c *Config) { c.OTATransport = srv })
	if err := h.n.SetBattery(0, false, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.n.Update(h.now); !errors.Is(err, ErrBatteryLow) {
		t.Fatalf("update on an empty battery: %v", err)
	}
	if len(srv.posts) != 0 {
		t.Error("it asked the server anyway")
	}
}

// TestPairingFromTheCrystalSendsItsFrames: Pair flushes what it queued,
// so a gesture that called it and then flushed again found nothing, and
// the first bond broadcast never went out.
func TestPairingFromTheCrystalSendsItsFrames(t *testing.T) {
	h := newHarness(t, nil)
	h.advance(bootDebounce)
	out := h.n.HoldFor(Crystal, crystalPairHold+100*time.Millisecond, h.now)
	if !h.n.Pairing() {
		t.Fatal("the hold did not start pairing")
	}
	if len(out) == 0 {
		t.Fatal("the hold sent no frames: the first bond request was dropped")
	}
	h.collect(out)
}

// TestGroupColorReachesTheCrystal: a Smart Group assigns this device a
// colour, and the crystal is what shows it. Setting only the config left
// the two saying different things.
func TestGroupColorReachesTheCrystal(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ColorID = int8(ColorBlue) })
	if got := h.n.LEDs().DefaultColor(); got != ColorBlue {
		t.Fatalf("crystal starts %s", got)
	}
	// Join first: a finalize only counts for the group this device is in.
	h.collect(h.n.Pair(h.now))
	h.take()
	beacon := mesh.SmartGroup{Instruction: mesh.SmartGroupAdvertise, UID: 55, TimeoutMs: 30000}
	h.rx(totem, mesh.Broadcast, -20, beacon)
	beacon.Members = []mesh.SmartGroupMember{{MAC: self}}
	h.rx(totem, mesh.Broadcast, -20, beacon)
	h.take()

	h.rx(totem, mesh.Broadcast, -20, mesh.SmartGroup{
		UID: 55, Instruction: mesh.SmartGroupFinalize, ColorID: int8(ColorRed),
		Members: []mesh.SmartGroupMember{{MAC: self, ColorID: int8(ColorRed)}},
	})
	if got := h.n.Config().ColorID; got != int8(ColorRed) {
		t.Fatalf("config colour = %d", got)
	}
	if got := h.n.LEDs().DefaultColor(); got != ColorRed {
		t.Errorf("crystal colour = %s, want the one the group gave", got)
	}
}
