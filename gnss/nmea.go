// Package gnss turns a receiver's NMEA sentences into position fixes.
//
// It is its own package, and plain Go, because the bytes it reads come off a
// wire this program does not control. A receiver emits a sentence every
// second forever, a cable can drop a character, and a module can be swapped
// for one that talks about satellites we have never heard of — so this is
// the part that has to be tested and fuzzed on a host rather than debugged
// on a board. Nothing here touches hardware; the board's driver hands it
// bytes and takes fixes back.
//
// Two sentences are read and the rest are ignored. RMC carries the date,
// the time, whether the solution is valid, the speed and the course; GGA
// carries the satellite count, the dilution of precision and the altitude.
// Between them they fill everything a Totem puts in a status frame.
package gnss

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Fix is one solution, in the units a Totem reports.
type Fix struct {
	Lat, Lon float32
	// AccuracyM is estimated from the horizontal dilution of precision;
	// see hdopAccuracyM. -1 when the receiver did not say.
	AccuracyM int8
	// AltitudeM is meters above mean sea level, or -500 for unknown, which
	// is the value the firmware uses for it.
	AltitudeM int16
	SpeedKPH  int8
	SatCount  int8
	// CourseDeg is the direction of travel, or -1 when the receiver gave
	// none — which it does when it is not moving, because a course needs
	// movement to mean anything.
	CourseDeg int16
	// Time is the receiver's clock, in UTC. Zero when the sentence carried
	// no usable date.
	Time time.Time
}

// ErrChecksum is returned for a sentence whose checksum does not match the
// bytes in front of it.
var ErrChecksum = errors.New("gnss: checksum does not match the sentence")

// maxSentence is the longest line the reader will hold. NMEA 0183 limits a
// sentence to 82 characters including the dollar and the line ending, and
// receivers do overrun it with proprietary messages, so there is room for
// twice that before a line is abandoned.
const maxSentence = 164

// Reader turns a byte stream into fixes. It is not safe for concurrent use;
// one receiver, one reader.
type Reader struct {
	line    [maxSentence]byte
	n       int
	dropped bool // this line overran, so discard it rather than truncate
	started bool // a dollar has been seen, so this is a sentence and not a tail

	// What the last GGA said. RMC is the sentence that completes a fix,
	// and it does not carry any of this.
	//
	// ggaAt is the sentence count when that GGA arrived, so its age can be
	// judged: zero means none has been seen. Counted in sentences rather
	// than seconds because this package is handed bytes, not a clock, and
	// a receiver emits its sentences in a fixed cycle — a GGA more than a
	// few sentences old is one that has stopped coming.
	sats      int8
	accuracy  int8
	altitude  int16
	ggaAt     uint64
	ggaNoFix  bool
	sentences uint64
	others    uint64
	bad       uint64

	// talker is the two letters the last good GGA came with. See Talker.
	talker string
}

// Talker is the talker ID of the last GGA the receiver got through, or ""
// before one arrives: GP for GPS alone, GN for a solution over several
// constellations, GL, GA or BD for one of those on its own.
//
// It is worth reporting because it says which satellites a receiver is
// allowed to use, which is the ceiling on the accuracy of everything built
// on its fixes. What a module solves from by default depends on the module
// and is read here rather than assumed: the bench T-Beam's MAX-M10S, sent
// nothing, reports GN from GPS, Galileo, BeiDou and QZSS.
func (r *Reader) Talker() string { return r.talker }

// ggaStaleAfter is how many sentences of interest a GGA's satellite count,
// accuracy and altitude stay attached to the fixes that follow it.
//
// Of interest, not of all kinds, and that distinction is the whole value of
// the number. A multi-constellation receiver sends GGA, then GLL, a GSA for
// each constellation and eight or nine GSVs, and only then RMC — fifteen or
// more sentences, none of which this package reads. Counting those made every
// GGA stale before the RMC it belonged to arrived, so outdoors with a good
// sky every fix reported no satellites, no accuracy and no altitude: exactly
// when those carry information. Counting only GGA and RMC, two is a cycle and
// four is generous.
const ggaStaleAfter = 4

// Sentences is how many sentences have parsed, whether or not this package
// had any use for them.
func (r *Reader) Sentences() uint64 { return r.sentences + r.others }

// Bad is how many were thrown away, for a checksum that did not match or a
// field that was not a number.
//
// The pair is worth having separately: a receiver that is wired up but
// mis-clocked shows as a rising Bad with Sentences standing still, which
// looks nothing like a receiver that is simply silent.
func (r *Reader) Bad() uint64 { return r.bad }

// Feed gives the reader one byte. It returns a fix when a sentence
// completes one, which is every RMC that says its solution is valid.
//
// A sentence that is not valid, not understood, or not checksummed
// correctly returns ok false: this is a stream to be read, not a request to
// be answered, and there is nothing to do about a bad line but wait for the
// next one.
func (r *Reader) Feed(b byte) (Fix, bool) {
	switch b {
	case '$':
		// A new sentence starts here whatever came before. A receiver
		// interrupted mid-line leaves a fragment, and the fragment must
		// not swallow the sentence after it.
		r.n, r.dropped, r.started = 0, false, true
		return Fix{}, false
	case '\r', '\n':
		line := r.line[:r.n]
		started, dropped := r.started, r.dropped
		r.n, r.dropped, r.started = 0, false, false
		if !started || dropped || len(line) == 0 {
			// Not started means this line began before the reader did, or
			// after one it gave up on: bytes with no dollar in front of
			// them are the tail of something, not a sentence. Refused
			// rather than parsed, even though the checksum would still
			// have to match — a tail is not a reading whatever it adds
			// up to, and it is not counted as a bad sentence either,
			// because arriving mid-stream is normal.
			return Fix{}, false
		}
		return r.sentence(string(line))
	}
	if r.n == len(r.line) {
		// Too long to be a sentence this package knows. Remember that,
		// so the tail is not read as a line of its own.
		r.dropped = true
		return Fix{}, false
	}
	r.line[r.n] = b
	r.n++
	return Fix{}, false
}

// FeedAll feeds a block of bytes and returns the last fix it produced. The
// last, not every one: the block a polled receiver hands over can hold
// several seconds of sentences, and the newest position is the only one
// worth reporting.
func (r *Reader) FeedAll(p []byte) (Fix, bool) {
	var (
		last Fix
		any  bool
	)
	for _, b := range p {
		if f, ok := r.Feed(b); ok {
			last, any = f, true
		}
	}
	return last, any
}

// sentence parses one complete sentence, without its dollar or line ending.
func (r *Reader) sentence(s string) (Fix, bool) {
	body, err := verify(s)
	if err != nil {
		r.bad++
		return Fix{}, false
	}
	fields := strings.Split(body, ",")
	if len(fields) == 0 || len(fields[0]) < 5 {
		r.bad++
		return Fix{}, false
	}
	// The first two letters are the talker — GP for GPS, GN for several
	// constellations at once, GL, GA, BD for one each. Nothing here decides
	// anything by it, but it is the only place the receiver says how many
	// constellations it is solving from, and that bounds the accuracy
	// everything downstream inherits: GP alone is one constellation and the
	// handful of satellites that go with it. Kept so it can be reported.
	//
	// Counted after the handler, not before it, so that a sentence is
	// either good or bad and never both. Counting here and then letting
	// gga or rmc reject a truncated one for its own reasons made both
	// climb together, which is what a healthy receiver with occasional
	// corruption looks like — the opposite of the reading Bad's own
	// documentation promises.
	switch fields[0][2:] {
	case "GGA":
		if !r.gga(fields) {
			return Fix{}, false
		}
		r.talker = fields[0][:2]
	case "RMC":
		fix, ok, good := r.rmc(fields)
		if !good {
			return Fix{}, false
		}
		r.sentences++
		return fix, ok
	default:
		// Read, understood to be none of our business, and not counted:
		// the counter is what the staleness window measures in, and a
		// receiver's GSVs are not a measure of how old its GGA is.
		r.others++
		return Fix{}, false
	}
	r.sentences++
	return Fix{}, false
}

// gga remembers the satellite count, the accuracy and the altitude, and
// says whether the sentence was one. A GGA too short to hold its fields is
// a bad sentence, not a GGA with nothing in it.
func (r *Reader) gga(f []string) bool {
	// $xxGGA,time,lat,ns,lon,ew,quality,sats,hdop,alt,altUnit,...
	if len(f) < 10 {
		r.bad++
		return false
	}
	// The one-based index this sentence is about to become: the counter is
	// bumped after the handler returns, and zero has to keep meaning "no
	// GGA yet".
	// Quality 0 is the receiver saying it has no solution. Its satellite
	// count and dilution then describe nothing, and attaching them to the
	// next valid RMC would advertise a solution quality to every peer that
	// this very sentence declined to vouch for. The staleness window below
	// catches a GGA that is old; this catches one that is negative.
	if quality, ok := decimal(f[6]); !ok || quality < 1 {
		// Remembered, not just discarded. A receiver indoors contradicts
		// itself: this GGA says it has no solution while the RMC beside it
		// says its position is valid, and on this bench that "valid"
		// position was 150 km away with no satellites behind it. The
		// sentence that admits it has nothing is the one to believe, so it
		// vetoes the one that does not for as long as it is current.
		r.ggaAt, r.ggaNoFix = r.sentences+1, true
		return true
	}
	r.ggaAt, r.ggaNoFix = r.sentences+1, false
	r.sats = int8(clampInt(atoiDefault(f[7], 0), 0, 127))
	r.accuracy = hdopAccuracyM(atofDefault(f[8], -1))
	r.altitude = -500
	if alt, ok := decimal(f[9]); ok {
		r.altitude = int16(clampFloat(alt, -500, 32767))
	}
	return true
}

// rmc completes a fix, if the receiver says its solution is valid.
// The second result says whether a fix came out; the third says whether the
// sentence was a sentence, which is what the counters need to know.
func (r *Reader) rmc(f []string) (fix Fix, gotFix, good bool) {
	// $xxRMC,time,status,lat,ns,lon,ew,knots,course,date,...
	if len(f) < 10 {
		r.bad++
		return Fix{}, false, false
	}
	if f[2] != "A" {
		// V, or anything else, means the receiver has no solution. Its
		// clock may still be good, but a position it does not vouch for
		// is not one to report. The sentence itself was fine.
		return Fix{}, false, true
	}
	if r.ggaSaysNoFix() {
		// The receiver's own GGA says it has no solution. Its RMC saying
		// otherwise is not a second opinion, it is the same receiver
		// disagreeing with itself, and a position with no satellites
		// behind it is not one to put on the air.
		return Fix{}, false, true
	}
	lat, latOK := latitude(f[3], f[4])
	lon, lonOK := longitude(f[5], f[6])
	if !latOK || !lonOK {
		r.bad++
		return Fix{}, false, false
	}
	fix = Fix{
		Lat: float32(lat), Lon: float32(lon),
		AccuracyM: -1, AltitudeM: -500, SatCount: 0, CourseDeg: -1,
		Time: utc(f[9], f[1]),
	}
	// What the last GGA said, but only if it was recent. A module
	// reconfigured to send RMC alone, or one whose GGA sentences start
	// failing their checksum while RMC survives, would otherwise have
	// every later fix stamped with a satellite count and an accuracy from
	// the last GGA it ever sent — and those go into the status frame as a
	// solution quality the receiver has no current evidence for.
	if r.ggaAt != 0 && r.sentences+1-r.ggaAt <= ggaStaleAfter {
		fix.AccuracyM, fix.AltitudeM, fix.SatCount = r.accuracy, r.altitude, r.sats
	}
	if knots, ok := decimal(f[7]); ok && knots >= 0 {
		// 1 knot is 1.852 km/h. Speed is a byte in the frame, so a
		// receiver reporting an airplane is pinned at its top value
		// rather than wrapping into a negative one.
		fix.SpeedKPH = int8(clampFloat(knots*1.852+0.5, 0, 127))
	}
	if course, ok := decimal(f[8]); ok && course >= 0 && course < 360 {
		fix.CourseDeg = int16(course + 0.5)
		if fix.CourseDeg == 360 {
			fix.CourseDeg = 0
		}
	}
	return fix, true, true
}

// verify checks the trailing *hh checksum, which is every byte between the
// dollar and the star, exclusive-ored together.
//
// A sentence with no checksum is refused rather than trusted. They are
// optional in NMEA and a receiver may leave them off, but this is the only
// thing standing between a dropped character on a serial line and a
// position a hundred miles out.
func verify(s string) (string, error) {
	star := strings.LastIndexByte(s, '*')
	// Exactly two digits, and then the end of the sentence. Accepting
	// trailing bytes let a line whose tail had been corrupted pass as a
	// good sentence on the strength of a checksum that covered less than
	// the line contained — and the checksum is the only thing standing
	// between a dropped character and a position a hundred miles out.
	if star < 0 || star+3 != len(s) {
		return "", ErrChecksum
	}
	want, err := strconv.ParseUint(s[star+1:star+3], 16, 8)
	if err != nil {
		return "", ErrChecksum
	}
	var got byte
	for i := range star {
		got ^= s[i]
	}
	if got != byte(want) {
		return "", ErrChecksum
	}
	return s[:star], nil
}

// latitude and longitude read the two coordinate fields. They are separate
// because the two are not interchangeable, and treating them as one let a
// fuzzed sentence through: 13352.0 with a hemisphere of S parsed as 133.87
// degrees south, which is past the pole and a third of the way up the other
// side of the planet. A latitude runs to 90 and takes N or S; a longitude
// runs to 180 and takes E or W.
func latitude(value, hemisphere string) (float64, bool) {
	return degrees(value, hemisphere, "N", "S", 90)
}

func longitude(value, hemisphere string) (float64, bool) {
	return degrees(value, hemisphere, "E", "W", 180)
}

// degrees turns NMEA's degrees-and-minutes into degrees. Latitude is
// ddmm.mmmm and longitude dddmm.mmmm, with the hemisphere in its own field.
func degrees(value, hemisphere, positive, negative string, limit float64) (float64, bool) {
	if value == "" || hemisphere == "" {
		return 0, false
	}
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		dot = len(value)
	}
	if dot < 3 {
		return 0, false
	}
	deg, ok := decimal(value[:dot-2])
	if !ok || deg < 0 {
		return 0, false
	}
	min, ok := decimal(value[dot-2:])
	if !ok || min < 0 || min >= 60 {
		return 0, false
	}
	d := deg + min/60
	switch hemisphere {
	case positive:
	case negative:
		d = -d
	default:
		// Including the other axis's letters: an N in a longitude field is
		// a sentence to throw away, not one to guess at.
		return 0, false
	}
	if d < -limit || d > limit {
		return 0, false
	}
	return d, true
}

// utc builds the time from RMC's ddmmyy date and hhmmss.ss time.
//
// The two-digit year is read as 2000 to 2099. NMEA has no century and the
// receivers this runs against were made in this one; a Totem that believed
// 1996 would hand every peer a clock they would refuse anyway.
func utc(date, clock string) time.Time {
	if len(date) != 6 || len(clock) < 6 {
		return time.Time{}
	}
	// Two digits each, and digits only. strconv.Atoi takes a sign, so a
	// field of "-1" parsed as a year gave 1999 — outside the 2000 to 2099
	// this promises, and past the check below, which only looks at whether
	// the date normalised. A clock of "12-530" likewise made the minutes
	// negative and moved the time back an hour. decimal exists to refuse
	// exactly this and was not being used here.
	day, okD := twoDigits(date[0:2])
	month, okM := twoDigits(date[2:4])
	year, okY := twoDigits(date[4:6])
	hour, okH := twoDigits(clock[0:2])
	minute, okMi := twoDigits(clock[2:4])
	sec, okS := twoDigits(clock[4:6])
	if !okD || !okM || !okY || !okH || !okMi || !okS {
		return time.Time{}
	}
	// 59, not 60. A sixtieth second passed and time.Date carried it into
	// the next minute, which the day-and-month check below cannot see — so
	// the fix arrived with a clock a minute ahead of the sentence, and that
	// clock is what the node re-slots its radio windows against and hands
	// to every peer that adopts it.
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}
	}
	t := time.Date(2000+year, time.Month(month), day, hour, minute, sec, 0, time.UTC)
	// Date normalises out of range days — 31 February becomes 2 March —
	// and a receiver that said that is not one to believe about the date.
	if t.Day() != day || int(t.Month()) != month {
		return time.Time{}
	}
	return t
}

// ggaSaysNoFix is whether a current GGA reported no solution.
func (r *Reader) ggaSaysNoFix() bool {
	return r.ggaNoFix && r.ggaAt != 0 && r.sentences+1-r.ggaAt <= ggaStaleAfter
}

// twoDigits reads exactly two decimal digits, and nothing else: no sign, no
// space, no letter.
func twoDigits(s string) (int, bool) {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

// hdopAccuracyM estimates the horizontal accuracy in meters from the
// dilution of precision.
//
// It is an estimate and it is stated as one. NMEA's GGA carries no accuracy
// figure, only HDOP, which is a geometry number: how much the satellites
// being badly spread multiplies whatever error the receiver already has.
// Multiplying it by 2.5 m — a consumer receiver's usual single-measurement
// error — is the common rule of thumb, and it is what turns a dilution into
// the meters a Totem reports. A real figure would come from a receiver's
// own estimate, which u-blox sends in a binary message this package does
// not read.
//
// Anything absent, negative or beyond the 99.99 that receivers send for
// "no idea" answers -1, which is how a Totem says it does not know.
func hdopAccuracyM(hdop float64) int8 {
	const metersPerHDOP = 2.5
	if hdop <= 0 || hdop >= 99 {
		return -1
	}
	return int8(clampInt(int(hdop*metersPerHDOP+0.5), 0, 127))
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func atofDefault(s string, def float64) float64 {
	f, ok := decimal(s)
	if !ok {
		return def
	}
	return f
}

// decimal reads a plain decimal number, and only that.
//
// strconv.ParseFloat is the wrong tool for a field off a wire, because it
// accepts more than a receiver can mean. "NaN" parses, and a NaN latitude
// passes every range check ever written — `d < -90 || d > 90` is false for
// it — so a checksum-valid sentence could hand back a fix at NaN, which is
// what the package doc promises never to do. "Inf", "0x1p112" and "1e300"
// parse too. None of them is a number a GNSS receiver produces; all of them
// are what a corrupted field or a hostile one looks like.
//
// So the shape is checked first: digits, at most one point, an optional
// leading sign, and nothing else. What survives that is safe to hand to
// ParseFloat, which is still needed for the actual conversion.
func decimal(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	body := s
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	digits, points := 0, 0
	for i := range len(body) {
		switch c := body[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			points++
		default:
			return 0, false
		}
	}
	if digits == 0 || points > 1 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

// clampFloat brings a value into range before it is converted to an integer,
// which is the only order that is safe.
//
// decimal accepts any run of digits, so a receiver — or a corrupted field —
// can present 1e20. Converting that to an int first and clamping after is
// undefined by the language: on a 64-bit host it comes out as the most
// negative integer, and clamping that to 0 turns the fastest speed ever
// reported into a standstill, while on the board's 32-bit int it may land
// anywhere at all, including inside the range. Clamping the float first
// leaves the conversion nothing to go wrong with.
func clampFloat(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Min(math.Max(v, lo), hi)
}
