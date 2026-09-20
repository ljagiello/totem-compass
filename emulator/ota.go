package emulator

// The WiFi OTA client, as f_ota/ and ota_daemon.py run it.
//
// A Totem updates over station WiFi: it asks api.totemportal.com what
// release it should be on, fetches the package index from the URL the
// answer gives, picks the .bin (firmware) or .tgz (preview) out of it,
// downloads it while the ring shows progress, writes it to the inactive
// slot and reboots. A triple tap of the SOS button starts it, as does a
// demi-god broadcast.
//
// Everything the server says is untrusted — the exchange is plain HTTP
// with no key, no signature and no TLS — so every field is checked here
// rather than believed. The emulator never writes a slot or reboots: it
// runs the exchange, counts the bytes and reports what a Totem would
// have installed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// The server contract, from f_ota/config.py and ota_callback.py.
const (
	// OTAEndpoint is API_ENDPOINT. Plain HTTP: there is no TLS on this
	// path, and no API key, so a server that answers these exchanges can
	// update a device that asks it.
	OTAEndpoint = "http://api.totemportal.com"
	// OTADeviceTypeID and OTAEndpointID are what the device reports about
	// itself in the release poll.
	OTADeviceTypeID = 1
	OTAEndpointID   = 1
	// contentsFile is the package index the release's URL serves.
	contentsFile = "contents.json"
)

// OTAState is where an update has got to.
type OTAState uint8

// The states, in the order an update passes through them.
const (
	OTAIdle OTAState = iota
	OTAChecking
	OTADownloading
	OTAVerifying
	OTAInstalling
	OTADone
	OTAFailed
)

// String names the state.
func (s OTAState) String() string {
	switch s {
	case OTAChecking:
		return "checking"
	case OTADownloading:
		return "downloading"
	case OTAVerifying:
		return "verifying"
	case OTAInstalling:
		return "installing"
	case OTADone:
		return "done"
	case OTAFailed:
		return "failed"
	}
	return "idle"
}

// LogValue names the state in a log line, in either format.
func (s OTAState) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Release is the release object the API returns, logged by the firmware
// as Product/Branch/Release Code/Release ID/OTA URL.
type Release struct {
	OTAURL      string `json:"ota_url"`
	Version     string `json:"version"`
	Product     string `json:"product"`
	Branch      string `json:"branch"`
	ReleaseCode string `json:"release_code"`
	ReleaseID   int    `json:"release_id"`
	// UpdateMethod is the "update method" field: firmware takes the .bin,
	// preview the .tgz.
	UpdateMethod string `json:"update_method"`
}

// releasePoll is the body of POST /devices/{MAC}/ota.
type releasePoll struct {
	Version      string  `json:"version"`
	EndpointID   int     `json:"endpoint_id"`
	DeviceTypeID int     `json:"device_type_id"`
	Lat          float32 `json:"lat"`
	Lon          float32 `json:"lon"`
	GNSSTime     int64   `json:"gnss_time"`
	ReleaseID    int     `json:"release_id"`
}

// releaseReport is the body of POST /devices/{MAC}/ota?updated, which the
// device sends once it is running the new image (record_release).
type releaseReport struct {
	BootCount    uint32  `json:"boot_count"`
	Branch       string  `json:"branch"`
	DeviceAge    int64   `json:"device_age"`
	DeviceTypeID int     `json:"device_type_id"`
	Product      string  `json:"product"`
	ReleaseCode  string  `json:"release_code"`
	ReleaseID    int     `json:"release_id"`
	Lat          float32 `json:"lat"`
	Lon          float32 `json:"lon"`
	GNSSTime     int64   `json:"gnss_time"`
}

// OTATransport is the HTTP the update needs. A board with a station
// connection provides one; the tests provide a fake; a board without
// WiFi has none, and then an update reports that rather than pretending.
type OTATransport interface {
	// Post sends a JSON body to a URL and returns the response body.
	Post(url string, body []byte) ([]byte, error)
	// Get fetches a URL.
	Get(url string) ([]byte, error)
	// Download streams a package, calling progress with the bytes so far
	// and the total, and returns the size and the SHA-256 the transport
	// saw. A device writes each block to the inactive slot as it arrives;
	// the emulator only counts them.
	Download(url string, progress func(done, total int64)) (size int64, sha256 string, err error)
}

// Errors an update can end with.
var (
	// ErrNoTransport means the board cannot reach a network.
	ErrNoTransport = errors.New("ota: this board has no network")
	// ErrBatteryLow is the firmware's "Battery too low for OTA update".
	ErrBatteryLow = errors.New("ota: battery too low for an update")
	// ErrNoPackage is ".bin package not found in repo, cannot perform OTA".
	ErrNoPackage = errors.New("ota: the repository holds no package of that kind")
	// ErrBadContents is "contents.json syntax err, cannot parse".
	ErrBadContents = errors.New("ota: contents.json does not parse")
	// ErrBadRelease means the release object is missing what an update needs.
	ErrBadRelease = errors.New("ota: the release is not usable")
	// ErrPoweredDown means the device has switched itself off, which is no
	// state to write a boot slot and reset from.
	ErrPoweredDown = errors.New("ota: the device is powered down")
)

// OTA runs one update at a time.
type OTA struct {
	state   OTAState
	release Release
	pkg     string
	// done and total are the download's progress in bytes.
	done, total int64
	err         error
	// took is how long the exchange ran, by the wall clock: the node's
	// own clock does not move for the length of a blocking update, so it
	// cannot measure one.
	took time.Duration
}

func newOTA() *OTA { return &OTA{} }

// State is where the last update got to.
func (o *OTA) State() OTAState { return o.state }

// Running reports whether an update is in flight — anywhere between
// asking the server and the reboot. The ring belongs to the update for
// all of it, not only while bytes are arriving.
func (o *OTA) Running() bool {
	switch o.state {
	case OTAChecking, OTADownloading, OTAVerifying, OTAInstalling:
		return true
	}
	return false
}

// Release is what the server said this device should be running.
func (o *OTA) Release() Release { return o.release }

// Err is why the last update failed, or nil.
func (o *OTA) Err() error { return o.err }

// Package is the file the update picked out of the index.
func (o *OTA) Package() string { return o.pkg }

// Progress is the download's share, 0 to 1.
func (o *OTA) Progress() float64 {
	if o.total <= 0 {
		return 0
	}
	return float64(o.done) / float64(o.total)
}

// Describe is the update's state for a console line.
func (o *OTA) Describe() string {
	switch o.state {
	case OTADownloading:
		return fmt.Sprintf("downloading %s: %d of %d bytes, %.0f%%", o.pkg, o.done, o.total, o.Progress()*100)
	case OTAFailed:
		return fmt.Sprintf("failed after %s: %v", o.took.Round(time.Millisecond), o.err)
	case OTADone:
		return fmt.Sprintf("installed %s (release %d) in %s", o.release.Version, o.release.ReleaseID,
			o.took.Round(time.Millisecond))
	case OTAIdle:
		return "idle"
	}
	return o.state.String()
}

// parseRelease reads the release object. A field the update needs, or a
// URL that is not a URL, is refused here rather than followed.
func parseRelease(b []byte) (Release, error) {
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, fmt.Errorf("%w: %w", ErrBadRelease, err)
	}
	if r.OTAURL == "" {
		return Release{}, fmt.Errorf("%w: no ota_url", ErrBadRelease)
	}
	if !strings.HasPrefix(r.OTAURL, "http://") && !strings.HasPrefix(r.OTAURL, "https://") {
		return Release{}, fmt.Errorf("%w: ota_url %q is not http", ErrBadRelease, r.OTAURL)
	}
	if strings.ContainsAny(r.OTAURL, " \t\r\n") {
		return Release{}, fmt.Errorf("%w: ota_url has whitespace in it", ErrBadRelease)
	}
	return r, nil
}

// parseContents reads the package index: a JSON array of file names.
func parseContents(b []byte) ([]string, error) {
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadContents, err)
	}
	for _, n := range names {
		// A name is a file in the repository, not a path out of it.
		if n == "" || strings.ContainsAny(n, "/\\") || strings.Contains(n, "..") {
			return nil, fmt.Errorf("%w: %q is not a file name", ErrBadContents, n)
		}
	}
	return names, nil
}

// pickPackage takes the entry with the wanted extension, as the firmware
// does: .bin for a firmware update, .tgz for a preview.
func pickPackage(names []string, ext string) (string, error) {
	for _, n := range names {
		if strings.HasSuffix(n, ext) {
			return n, nil
		}
	}
	return "", fmt.Errorf("%w: no %s in the index", ErrNoPackage, ext)
}

// versionFromName reads the version out of a package name: the firmware
// takes what lies between "_v" and the extension, as in
// firmware_v5.0.3.bin. A name without it has no version, which is worth
// saying rather than guessing.
func versionFromName(name string) (string, error) {
	i := strings.LastIndex(name, "_v")
	if i < 0 {
		return "", fmt.Errorf("%q has no _v", name)
	}
	rest := name[i+2:]
	// The version ends where the package extension begins. A name with no
	// extension is not a package, and guessing where the version stops
	// would take the "3" off a 5.0.3.
	var v string
	switch {
	case strings.HasSuffix(rest, ".bin"):
		v = strings.TrimSuffix(rest, ".bin")
	case strings.HasSuffix(rest, ".tgz"):
		v = strings.TrimSuffix(rest, ".tgz")
	default:
		return "", fmt.Errorf("%q does not end in .bin or .tgz", name)
	}
	if v == "" {
		return "", fmt.Errorf("%q has an empty version", name)
	}
	return v, nil
}

// extFor is the package extension an update method uses.
func extFor(method string) string {
	if strings.EqualFold(method, "preview") {
		return ".tgz"
	}
	return ".bin"
}

// Update runs one update: poll, index, download, verify, install. It
// reports what a Totem would have installed; nothing is written to flash
// and the device does not reboot.
//
// It is a single call rather than a state machine because the transport
// blocks anyway, and a Totem is not doing anything else while it updates:
// the firmware's own OTA holds off the watchdog and the radio.
func (n *Node) Update(now time.Time) error {
	o := n.ota
	if o.Running() {
		return errors.New("ota: an update is already running")
	}
	o.state, o.err, o.done, o.total = OTAChecking, nil, 0, 0
	// The touch blocks this update takes, so they can be given back
	// exactly as they were found.
	var blocked []time.Time
	// The exchange blocks, and the node's clock does not move while it
	// does, so the time it took is measured against the wall. It is only
	// ever reported, never used to decide anything.
	began := time.Now()
	fail := func(err error) error {
		o.state, o.err, o.took = OTAFailed, err, time.Since(began)
		n.log.Warn("ota failed", "err", err)
		n.leds.Play(AnimOTAFailed, now)
		n.unblockTouch(blocked)
		return err
	}
	// A device that has powered itself down is not a device to reboot
	// into a new image: its radio windows have stopped, and on a board
	// this would write a boot slot and reset.
	if n.power.Off() {
		return fail(ErrPoweredDown)
	}
	// The battery gate comes next: an update that reboots a device with a
	// flat battery is how a device does not come back. 0% is the flattest
	// reading there is, so it belongs inside the gate, and so does Low
	// from a charger chip that reports no percentage at all. Only a board
	// with no power chip is exempt, and only because it was configured to
	// say so — never because it happens to report zeroes, which a failed
	// reading and a dead cell also do, and those are the cases the gate
	// exists for.
	b := n.sensors.Battery
	if !b.NoPowerChip && !b.Charging && (b.Low || b.Percent < otaMinPct) {
		return fail(fmt.Errorf("%w: %d%%", ErrBatteryLow, b.Percent))
	}
	if n.cfg.OTATransport == nil {
		return fail(ErrNoTransport)
	}
	t := n.cfg.OTATransport
	n.leds.Play(AnimWiFi, now)

	poll := releasePoll{
		Version: n.versionString(), EndpointID: OTAEndpointID, DeviceTypeID: OTADeviceTypeID,
		ReleaseID: int(n.cfg.ReleaseID),
	}
	if f := n.fix(); f != nil {
		poll.Lat, poll.Lon = f.Lat, f.Lon
		if !f.Time.IsZero() {
			poll.GNSSTime = f.Time.Unix()
		}
	}
	body, err := json.Marshal(poll)
	if err != nil {
		return fail(err)
	}
	url := fmt.Sprintf("%s/devices/%s/ota", OTAEndpoint, strings.ToUpper(n.cfg.MAC.String()))
	answer, err := t.Post(url, body)
	if err != nil {
		return fail(fmt.Errorf("ota: asking %s: %w", url, err))
	}
	rel, err := parseRelease(answer)
	if err != nil {
		return fail(err)
	}
	o.release = rel
	n.log.Info("ota release", "product", rel.Product, "branch", rel.Branch,
		"release_code", rel.ReleaseCode, "release_id", rel.ReleaseID, "url", rel.OTAURL)

	index, err := t.Get(strings.TrimSuffix(rel.OTAURL, "/") + "/" + contentsFile)
	if err != nil {
		return fail(fmt.Errorf("ota: fetching %s: %w", contentsFile, err))
	}
	names, err := parseContents(index)
	if err != nil {
		return fail(err)
	}
	pkg, err := pickPackage(names, extFor(rel.UpdateMethod))
	if err != nil {
		return fail(err)
	}
	o.pkg = pkg
	if v, err := versionFromName(pkg); err == nil {
		n.log.Info("ota package", "file", pkg, "version", v)
	} else {
		n.log.Warn("ota package name carries no version", "file", pkg, "err", err)
	}

	o.state = OTADownloading
	n.leds.Play(AnimOTA, now)
	// Block Touch while the package comes down: a tap part way through an
	// update is not a gesture anyone means.
	blocked = n.blockTouch(now, otaTouchBlock)
	size, sum, err := t.Download(strings.TrimSuffix(rel.OTAURL, "/")+"/"+pkg, func(done, total int64) {
		o.done, o.total = done, total
		if total > 0 {
			n.leds.SetProgress(float64(done) / float64(total))
		}
		// Draw here: an update blocks whichever loop called it, so this
		// is the only chance the progress ring gets to move while the
		// package comes down. The touch block is renewed with it, so a
		// download slower than the block does not leave the inputs live
		// halfway through.
		at := now.Add(time.Since(began))
		n.leds.Tick(at)
		n.blockTouch(at, otaTouchBlock)
	})
	if err != nil {
		return fail(fmt.Errorf("ota: downloading %s: %w", pkg, err))
	}
	if size <= 0 {
		return fail(fmt.Errorf("ota: %s is empty", pkg))
	}
	o.state = OTAVerifying
	n.log.Info("ota downloaded", "file", pkg, "bytes", size, "sha256", sum)

	// A Totem writes the image to the inactive slot and reboots into it,
	// with the bootloader's own hash check deciding whether it stays. The
	// emulator stops here: it has one image, and losing it would take the
	// board off the mesh.
	o.state, o.took = OTADone, time.Since(began)
	n.leds.Play(AnimIdle, now)
	n.unblockTouch(blocked)
	n.log.Info("ota complete", "installed", false,
		"note", "the emulator runs the exchange but does not write a slot or reboot")
	return nil
}

// otaTouchBlock is how long the inputs stay blocked once a download
// starts. It is refreshed while the download runs and released when it
// ends, so a stalled update does not leave the device deaf.
const otaTouchBlock = 30 * time.Second

// blockTouch is the firmware's Block Touch, which it holds while
// something must not be interrupted. It returns what the blocks were, so
// unblockTouch can put them back rather than clearing whatever else was
// holding an input — the wait after a boot, most of all.
func (n *Node) blockTouch(now time.Time, d time.Duration) []time.Time {
	was := make([]time.Time, len(n.inputs))
	for i := range n.inputs {
		was[i] = n.inputs[i].blockedUntil
		n.inputs[i].block(now, d)
	}
	return was
}

// unblockTouch puts back the deadlines blockTouch found.
func (n *Node) unblockTouch(was []time.Time) {
	for i := range n.inputs {
		if i < len(was) {
			n.inputs[i].blockedUntil = was[i]
		}
	}
}

// versionString is the release_code a Totem reports, "5.0.3".
func (n *Node) versionString() string {
	return fmt.Sprintf("%d.%d.%d", n.cfg.Version[0], n.cfg.Version[1], n.cfg.Version[2])
}

// StartOTA is the SOS button's triple tap (sw_sos.cb_triple_tap ->
// start_ota) and the demi-god update command. Update logs what went
// wrong, and a gesture has nowhere to return an error to.
func (n *Node) StartOTA(now time.Time) { _ = n.Update(now) }

// OTA reports the update's state.
func (n *Node) OTA() *OTA { return n.ota }

// OTAReport is the body a Totem posts to /devices/{MAC}/ota?updated once
// it is running the new image (record_release). The emulator never
// installs one, so it never sends this; it is here because a server on
// the other side has to accept it, and because a test can then pin the
// field names a server will see.
func (n *Node) OTAReport(boots uint32, now time.Time) ([]byte, error) {
	rel := n.ota.Release()
	r := releaseReport{
		BootCount: boots, Branch: rel.Branch, DeviceAge: int64(now.Sub(n.boot).Seconds()),
		DeviceTypeID: OTADeviceTypeID, Product: rel.Product,
		ReleaseCode: rel.ReleaseCode, ReleaseID: rel.ReleaseID,
	}
	if f := n.fix(); f != nil {
		r.Lat, r.Lon = f.Lat, f.Lon
		if !f.Time.IsZero() {
			r.GNSSTime = f.Time.Unix()
		}
	}
	return json.Marshal(r)
}
