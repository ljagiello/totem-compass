package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ljagiello/totem-compass/protocol"
)

// printer writes results to out (human text or JSON lines) and progress to
// err, so -json output stays machine-readable.
type printer struct {
	json  bool
	quiet bool
	out   io.Writer // stdout
	err   io.Writer // stderr
	log   *slog.Logger
}

// printf and println write results. Write errors on stdout/stderr are not
// actionable for a CLI, so they are dropped here, in one place.
func (p *printer) printf(format string, args ...any) { _, _ = fmt.Fprintf(p.out, format, args...) }
func (p *printer) println(args ...any)               { _, _ = fmt.Fprintln(p.out, args...) }

func (p *printer) status(format string, args ...any) {
	if !p.quiet {
		_, _ = fmt.Fprintf(p.err, format+"\n", args...)
	}
}

// emit writes v as one JSON line tagged with its type.
//
// A value JSON cannot hold still produces a line, saying what could not
// be written and what type it was.
//
// This is a backstop, not the fix for a hole any caller has: the
// protocol decoder already puts every float it hands out through
// finite(), for this reason, and mesh frames are handled where they are
// decoded — there the raw hex survives, which matters more than the
// type name. What this catches is a value that reaches JSON without
// having been through either, which is a shape this tool grows every
// time it prints something new.
func (p *printer) emit(v any) {
	t := reflect.TypeOf(v)
	b, err := json.Marshal(struct {
		Type string `json:"type"`
		Data any    `json:"data"`
	}{t.Name(), v})
	if err == nil {
		p.println(string(b))
		return
	}
	p.log.Warn("cannot encode as JSON", "type", t.Name(), "err", err)
	// Two strings, which encoding/json cannot refuse: an invalid byte in
	// one is escaped rather than returned as an error.
	b, _ = json.Marshal(struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}{t.Name(), err.Error()})
	p.println(string(b))
}

// message prints one received message as a single line.
func (p *printer) message(m protocol.Message) {
	if _, ok := m.(protocol.Handoff); ok {
		return // TX handoffs are protocol housekeeping; --trace shows them
	}
	if p.json {
		p.emit(m)
		return
	}
	ts := time.Now().Format("15:04:05")
	switch v := m.(type) {
	case protocol.LiveData:
		p.printf("%s live   %s\n", ts, liveLine(v))
	case protocol.StaticData:
		p.printf("%s static %q v%s release %d, mac %s, colour %s, compass lock %s, persistent north %s\n",
			ts, v.Name, v.Version, v.ReleaseID, v.MAC.Pretty(), protocol.ColorName(v.ColorID), onOff(v.CompassLock), onOff(v.PersistentNorth))
	case protocol.PeerPing:
		p.printf("%s peer   %s\n", ts, peerLine(v))
	case protocol.PeerSync:
		macs := make([]string, len(v.Peers))
		for i, m := range v.Peers {
			macs[i] = m.String()
		}
		p.printf("%s peers  %d bonded: %s\n", ts, len(v.Peers), strings.Join(macs, " "))
	case protocol.WiFiNetworks:
		p.printf("%s wifi   %s\n", ts, strings.Join(v.SSIDs, ", "))
	case protocol.FileChunk:
		p.printf("%s file   %q chunk %d, %d/%d bytes, status %d action %d\n", ts, v.Name, v.ChunkNo, v.BytePos, v.FileSize, v.Status, v.Action)
	case protocol.DisconnectIntent:
		p.printf("%s device is about to disconnect (cmd %d, %v)\n", ts, v.Cmd, v.Params)
	case protocol.Unknown:
		p.printf("%s ?      %s (%d,%d) % x\n", ts, v.Channel, v.Cat, v.Cmd, v.Raw)
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func flagList(pairs ...any) string {
	var set []string
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i+1].(bool) {
			set = append(set, pairs[i].(string))
		}
	}
	if len(set) == 0 {
		return ""
	}
	return " [" + strings.Join(set, ",") + "]"
}

func liveLine(v protocol.LiveData) string {
	var b strings.Builder
	if v.HasLocation {
		fmt.Fprintf(&b, "%.6f,%.6f", v.Lat, v.Lon)
		if v.PosAccuracyM != nil {
			fmt.Fprintf(&b, " ±%.1fm", *v.PosAccuracyM)
		}
		if v.AltitudeM != nil {
			fmt.Fprintf(&b, " alt %dm", *v.AltitudeM)
		}
	} else {
		b.WriteString("no fix")
	}
	fmt.Fprintf(&b, "  sats %d  azimuth %d°  heading %d°", v.SatCount, v.Azimuth, v.HeadingMot)
	if v.SpeedKPH != nil {
		fmt.Fprintf(&b, "  %dkm/h", *v.SpeedKPH)
	}
	fmt.Fprintf(&b, "  batt %d%% %.2fV  power %s", v.BattPct, v.BattVolts, reportedPower(v.PowerMode))
	if !v.Time.IsZero() {
		fmt.Fprintf(&b, "  clock %s", v.Time.Format("15:04:05"))
	}
	b.WriteString(flagList("sos", v.SOS, "charging", v.Charging, "low-battery", v.LowBattery, "eco", v.Eco, "mag-cal-needed", v.MagCalNeeded))
	return b.String()
}

func peerLine(v protocol.PeerPing) string {
	kind := "peer"
	if v.POI {
		kind = "poi"
	}
	s := fmt.Sprintf("%s %-4s %-16q", v.MAC, kind, v.Name)
	if v.Lat != 0 || v.Lon != 0 {
		s += fmt.Sprintf("  %.6f,%.6f", v.Lat, v.Lon)
		if !v.LastCoords.IsZero() {
			s += " @" + v.LastCoords.Format("Jan 2 15:04")
		}
	} else {
		s += "  no position"
	}
	if v.Bearing >= 0 {
		s += fmt.Sprintf("  bearing %d°", v.Bearing)
	}
	if v.RSSI != 100 {
		s += fmt.Sprintf("  rssi %d", v.RSSI)
	}
	if !v.POI {
		s += fmt.Sprintf("  batt %d%%", v.BattPct)
	}
	s += "  colour " + v.Color.String()
	return s + flagList("sos", v.SOS, "via-mesh", v.ViaMesh, "stale", v.Stale, "hidden", v.Hidden, "locked", v.Locked)
}

func (p *printer) info(st protocol.StaticData, live *protocol.LiveData) {
	if p.json {
		p.emit(st)
		if live != nil {
			p.emit(*live)
		}
		return
	}
	w := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	row := func(k string, v any) { _, _ = fmt.Fprintf(w, "%s\t%v\n", k, v) }
	row("Name", st.Name)
	row("MAC", st.MAC.Pretty())
	row("Firmware", fmt.Sprintf("%s (release %d, branch %s)", st.Version, st.ReleaseID, st.Branch))
	row("Colour", protocol.ColorName(st.ColorID))
	row("Compass lock", onOff(st.CompassLock))
	row("Persistent north", onOff(st.PersistentNorth))
	row("WiFi network", orNone(st.WiFiSSID))
	row("Half duplex", map[bool]string{true: "supported (--half-duplex)", false: "not supported"}[st.HalfDuplex])
	if live != nil {
		row("Battery", fmt.Sprintf("%d%% (%.2f V)%s%s", live.BattPct, live.BattVolts,
			map[bool]string{true: ", charging"}[live.Charging], map[bool]string{true: ", low"}[live.LowBattery]))
		row("Power mode", reportedPower(live.PowerMode))
		if live.HasLocation {
			pos := fmt.Sprintf("%.6f, %.6f", live.Lat, live.Lon)
			if live.PosAccuracyM != nil {
				pos += fmt.Sprintf(" (±%.1f m)", *live.PosAccuracyM)
			}
			row("Position", pos)
		} else {
			row("Position", "no fix")
		}
		row("Satellites", live.SatCount)
		row("Compass azimuth", fmt.Sprintf("%d°", live.Azimuth))
		row("ESP-NOW channel", live.Channel)
		row("Uptime", live.Uptime)
		if !live.Time.IsZero() {
			row("Device clock", live.Time.Local().Format(time.RFC1123))
		}
		if live.MagCalNeeded {
			row("Magnetometer", "calibration needed")
		}
		if live.SOS {
			row("SOS", "ACTIVE")
		}
	}
	_ = w.Flush()
}

// reportedPower renders a power mode the device reported; 0 means the
// firmware did not fill the field in (4.x does not).
func reportedPower(p protocol.PowerMode) string {
	if p == protocol.PowerUnchanged {
		return "not reported"
	}
	return p.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
