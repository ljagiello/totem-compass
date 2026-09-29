package gnss

import (
	"math"
	"strings"
	"testing"
	"time"
)

// feed is the reader given a whole string, as a receiver would send it.
func feed(t *testing.T, r *Reader, s string) (Fix, bool) {
	t.Helper()
	return r.FeedAll([]byte(s))
}

// TestNoFixIndoors is what the board's own receiver actually said, read off
// its serial port with no sky in sight. It is here because it is the first
// thing the driver sees every time it starts, and because the status field
// is the one that decides whether there is a position at all: both these
// sentences carry a real time and no location.
func TestNoFixIndoors(t *testing.T) {
	var r Reader
	const said = "$GNRMC,001655.00,V,,,,,,,290926,,,N,V*18\r\n" +
		"$GNGGA,001655.00,,,,,0,00,99.99,,,,,,*7F\r\n"
	if fix, ok := feed(t, &r, said); ok {
		t.Errorf("a receiver with no solution produced a fix: %+v", fix)
	}
	if r.Sentences() != 2 {
		t.Errorf("read %d sentences, want 2", r.Sentences())
	}
	if r.Bad() != 0 {
		t.Errorf("%d sentences were rejected, want none: they came off the board", r.Bad())
	}
}

func TestFix(t *testing.T) {
	var r Reader
	// GGA first, as a receiver sends it: the satellites, the dilution and
	// the altitude come from there, and RMC completes the fix.
	if _, ok := feed(t, &r, "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n"); ok {
		t.Fatal("GGA alone produced a fix; only RMC vouches for a solution")
	}
	fix, ok := feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,003.1,W*63\r\n")
	if !ok {
		t.Fatal("a valid RMC produced no fix")
	}
	// 4807.038 N is 48 degrees and 7.038 minutes.
	if got, want := fix.Lat, float32(48+7.038/60); !near(float64(got), float64(want), 1e-5) {
		t.Errorf("latitude %v, want %v", got, want)
	}
	if got, want := fix.Lon, float32(11+31.0/60); !near(float64(got), float64(want), 1e-5) {
		t.Errorf("longitude %v, want %v", got, want)
	}
	if got, want := fix.SatCount, int8(8); got != want {
		t.Errorf("satellites %d, want %d", got, want)
	}
	// 0.9 HDOP at 2.5 m each is 2.25, which rounds to 2.
	if got, want := fix.AccuracyM, int8(2); got != want {
		t.Errorf("accuracy %d m, want %d", got, want)
	}
	if got, want := fix.AltitudeM, int16(545); got != want {
		t.Errorf("altitude %d m, want %d", got, want)
	}
	// 22.4 knots is 41.5 km/h.
	if got, want := fix.SpeedKPH, int8(41); got != want {
		t.Errorf("speed %d km/h, want %d", got, want)
	}
	if got, want := fix.CourseDeg, int16(84); got != want {
		t.Errorf("course %d degrees, want %d", got, want)
	}
	want := time.Date(2026, 3, 23, 12, 35, 19, 0, time.UTC)
	if !fix.Time.Equal(want) {
		t.Errorf("time %v, want %v", fix.Time, want)
	}
}

func TestSouthAndWestAreNegative(t *testing.T) {
	var r Reader
	fix, ok := feed(t, &r, "$GPRMC,123519,A,3352.000,S,15112.000,W,000.0,000.0,230326,,*18\r\n")
	if !ok {
		t.Fatal("a southern, western fix was refused")
	}
	if fix.Lat >= 0 {
		t.Errorf("latitude %v in the southern hemisphere, want negative", fix.Lat)
	}
	if fix.Lon >= 0 {
		t.Errorf("longitude %v in the western hemisphere, want negative", fix.Lon)
	}
}

// TestChecksumIsEnforced: a dropped or altered character must not become a
// position. One serial glitch in a latitude field is a fix hundreds of
// kilometers away, and the checksum is the only thing that catches it.
func TestChecksumIsEnforced(t *testing.T) {
	const good = "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,003.1,W*63\r\n"
	// Same sentence with one digit of the latitude changed, checksum left
	// as it was — which is exactly what a corrupted byte looks like.
	bad := strings.Replace(good, "4807.038", "4907.038", 1)
	for _, tt := range []struct {
		name string
		line string
		want bool
	}{
		{"as sent", good, true},
		{"a byte changed", bad, false},
		{"checksum removed", strings.Replace(good, "*63", "", 1), false},
		{"checksum truncated", strings.Replace(good, "*63", "*6", 1), false},
		{"checksum not hex", strings.Replace(good, "*63", "*zz", 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r Reader
			if _, ok := feed(t, &r, tt.line); ok != tt.want {
				t.Errorf("fix = %v, want %v", ok, tt.want)
			}
		})
	}
}

// TestFragmentDoesNotSwallowTheNextSentence: a receiver switched on
// mid-sentence, or a line lost to a full buffer, leaves a fragment. The
// sentence after it has to still be read.
func TestFragmentDoesNotSwallowTheNextSentence(t *testing.T) {
	// The tail on its own is not a sentence, whatever it adds up to: a
	// reader started mid-stream sees one every time it is switched on, and
	// bytes with no dollar in front of them are not a reading.
	var tail Reader
	if _, ok := feed(t, &tail, "038,N,01131.000,E,1,08,0.9,545.4,M,,*11\r\n"); ok {
		t.Error("a tail with no start was read as a sentence")
	}
	if tail.Bad() != 0 {
		t.Errorf("the tail counted as %d bad sentences; arriving mid-stream is normal", tail.Bad())
	}

	var r Reader
	const stream = "038,N,01131.000,E,1,08,0.9,545.4,M,,*11\r\n" + // a tail with no start
		"$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n"
	if _, ok := feed(t, &r, stream); !ok {
		t.Error("the sentence after a fragment was lost")
	}

	// And a dollar in the middle of a line starts the sentence over,
	// rather than the leading rubbish making it unparseable.
	var r2 Reader
	if _, ok := feed(t, &r2, "rubbish$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n"); !ok {
		t.Error("a sentence preceded by rubbish on the same line was lost")
	}
}

// TestOverlongLineIsDropped: a sentence longer than the buffer must be
// abandoned whole. Truncating it would leave a line whose checksum happens
// to cover fewer bytes than it claims.
func TestOverlongLineIsDropped(t *testing.T) {
	var r Reader
	long := "$GPRMC," + strings.Repeat("9", maxSentence*2) + ",A,4807.038,N,01131.000,E,,,230326,,*00\r\n"
	if _, ok := feed(t, &r, long); ok {
		t.Error("an overlong sentence produced a fix")
	}
	// The next one still works.
	if _, ok := feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n"); !ok {
		t.Error("the sentence after an overlong one was lost")
	}
}

func TestUnknownSentencesAreCounted(t *testing.T) {
	// Counted as read, and not as bad — but kept apart from the sentences
	// this package acts on, because those are what the GGA staleness window
	// is measured in. See TestStaleGGAIsNotAttached.
	var r Reader
	// A GSV is a real sentence this package has no use for. It should be
	// counted as read, not as bad.
	if _, ok := feed(t, &r, "$GPGSV,3,1,11,03,03,111,00,04,15,270,00,06,01,010,00,13,06,292,00*74\r\n"); ok {
		t.Error("GSV produced a fix")
	}
	if r.Sentences() != 1 || r.Bad() != 0 {
		t.Errorf("sentences=%d bad=%d, want 1 and 0: a sentence we ignore still parsed",
			r.Sentences(), r.Bad())
	}
}

// TestSpeedAndCourseOutOfRange: the frame carries speed in a byte and a
// course as a bearing, so a receiver reporting something absurd must be
// pinned rather than wrapped into a negative.
func TestSpeedAndCourseOutOfRange(t *testing.T) {
	var r Reader
	// 9999 knots, and a course of 359.7 which rounds to 360.
	fix, ok := feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,9999.0,359.7,230326,,*2C\r\n")
	if !ok {
		t.Fatal("the absurd-but-well-formed sentence was refused")
	}
	if fix.SpeedKPH != 127 {
		t.Errorf("speed %d, want it pinned at 127", fix.SpeedKPH)
	}
	if fix.CourseDeg != 0 {
		t.Errorf("course %d, want 359.7 rounded to 0 rather than 360", fix.CourseDeg)
	}
}

func TestHDOPAccuracy(t *testing.T) {
	for _, tt := range []struct {
		hdop float64
		want int8
	}{
		{-1, -1},
		{0, -1},
		{0.9, 2},
		{1.0, 3},
		{2.0, 5},
		{99.99, -1}, // what a receiver sends for "no idea"
		{1000, -1},
	} {
		if got := hdopAccuracyM(tt.hdop); got != tt.want {
			t.Errorf("hdopAccuracyM(%v) = %d, want %d", tt.hdop, got, tt.want)
		}
	}
}

func TestBadDatesAreRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		date  string
		clock string
		zero  bool
	}{
		{"as sent", "230326", "123519", false},
		{"31 February", "310226", "123519", true},
		{"month 13", "011326", "123519", true},
		{"day 0", "000326", "123519", true},
		{"hour 25", "230326", "253519", true},
		// A sixtieth second passed and time.Date carried it into the next
		// minute, which the day-and-month check cannot see.
		{"second 60", "230326", "123560", true},
		{"second 59", "230326", "123559", false},
		{"short date", "2303", "123519", true},
		{"short time", "230326", "1235", true},
		{"not numbers", "23xx26", "123519", true},
		// strconv.Atoi takes a sign, so a signed field used to parse: a
		// year of "-1" gave 1999, which is outside the century this reads
		// two digits as and past the normalisation check below it.
		{"signed year", "1203-1", "123519", true},
		{"signed minute", "230326", "12-530", true},
		{"spaced", "23 326", "123519", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := utc(tt.date, tt.clock)
			if got.IsZero() != tt.zero {
				t.Errorf("utc(%q, %q) = %v, want zero == %v", tt.date, tt.clock, got, tt.zero)
			}
		})
	}
}

func TestLatitudeRefusesNonsense(t *testing.T) {
	for _, tt := range []struct {
		value, hemi, why string
	}{
		{"", "N", "no value"},
		{"4807.038", "", "no hemisphere"},
		{"4807.038", "X", "not a hemisphere"},
		{"4807.038", "E", "a longitude's hemisphere in a latitude"},
		{"12", "N", "too short to hold minutes"},
		{"4899.999", "N", "99 minutes is not a thing"},
		{"abcd.efg", "N", "not a number"},
		{"48o7.038", "N", "a letter where a digit was"},
		// What the fuzzer found. Read with a longitude's range this is
		// 133.87 degrees south, a third of the way up the far side of the
		// planet, and it went into a fix.
		{"13352.0", "S", "past the pole"},
		{"9000.001", "N", "just past the pole"},
	} {
		if got, ok := latitude(tt.value, tt.hemi); ok {
			t.Errorf("latitude(%q, %q) = %v, accepted despite %s", tt.value, tt.hemi, got, tt.why)
		}
	}
	// The poles themselves are places.
	if _, ok := latitude("9000.000", "S"); !ok {
		t.Error("the south pole was refused")
	}
}

func TestLongitudeRefusesNonsense(t *testing.T) {
	for _, tt := range []struct {
		value, hemi, why string
	}{
		{"", "E", "no value"},
		{"01131.000", "", "no hemisphere"},
		{"01131.000", "N", "a latitude's hemisphere in a longitude"},
		{"18100.000", "E", "past the antimeridian"},
		{"12", "E", "too short to hold minutes"},
	} {
		if got, ok := longitude(tt.value, tt.hemi); ok {
			t.Errorf("longitude(%q, %q) = %v, accepted despite %s", tt.value, tt.hemi, got, tt.why)
		}
	}
	// A longitude may go where a latitude may not.
	if _, ok := longitude("13352.0", "W"); !ok {
		t.Error("133 degrees west was refused, and it is a real place")
	}
}

// TestFuzzedLatitudeSentence is the whole sentence the fuzzer built, kept
// because the bug it found was not in the field parser's own arithmetic but
// in one range check standing in for two.
func TestFuzzedLatitudeSentence(t *testing.T) {
	var r Reader
	if fix, ok := feed(t, &r, "$GPRMC,12359,A,13352.0,S,152.0,W,230326,,*18\r\n"); ok {
		t.Errorf("a latitude past the pole became a fix: %+v", fix)
	}
}

func near(a, b, tol float64) bool { return a-b < tol && b-a < tol }

// TestNotANumberIsNotAPlace: a field is only a number if it looks like one.
// strconv.ParseFloat accepts "NaN", "Inf" and "0x1p112", and a NaN latitude
// passes every range check ever written — `d < -90 || d > 90` is false for
// it — so a sentence whose checksum happens to match could hand back a fix
// at NaN, which is what this package promises never to do. The checksums
// below are real, which is the point: nothing else would have stopped them.
func TestNotANumberIsNotAPlace(t *testing.T) {
	for _, tt := range []struct {
		name, line string
	}{
		{"NaN latitude", "$GPRMC,123519,A,NaN12.0,N,01131.000,E,022.4,084.4,230326,003.1,W*01\r\n"},
		{"hex float longitude", "$GPRMC,123519,A,4807.038,N,0x1p112.0,E,022.4,084.4,230326,,*11\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r Reader
			fix, ok := feed(t, &r, tt.line)
			if ok {
				t.Fatalf("produced a fix at %v, %v", fix.Lat, fix.Lon)
			}
		})
	}

	// The same for the fields that are not coordinates: a NaN speed or a
	// NaN dilution must not reach the arithmetic either, where converting
	// it to an integer is not even defined.
	var r Reader
	fix, ok := feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,NaN,084.4,230326,,*53\r\n")
	if !ok {
		t.Fatal("a sentence with one bad field lost its whole fix")
	}
	if fix.SpeedKPH != 0 {
		t.Errorf("speed %d from a NaN field, want it left alone", fix.SpeedKPH)
	}
	var r2 Reader
	feed(t, &r2, "$GPGGA,123519,4807.038,N,01131.000,E,1,08,NaN,545.4,M,46.9,M,,*01\r\n")
	if got := r2.accuracy; got != -1 {
		t.Errorf("accuracy %d from a NaN dilution, want -1", got)
	}
}

func TestDecimal(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"1", true}, {"1.5", true}, {"-1.5", true}, {"+1.5", true},
		{"0001131.000", true}, {".5", true}, {"5.", true},
		{"", false}, {"NaN", false}, {"nan", false}, {"Inf", false},
		{"-Inf", false}, {"0x1p112", false}, {"1e300", false},
		{"1.2.3", false}, {"1 ", false}, {" 1", false}, {"--1", false},
		{".", false}, {"abc", false},
	} {
		if _, ok := decimal(tt.in); ok != tt.want {
			t.Errorf("decimal(%q) ok = %v, want %v", tt.in, ok, tt.want)
		}
	}
}

// TestStaleGGAIsNotAttached: a receiver that stops sending GGA must not
// have every later fix stamped with the satellite count and accuracy of the
// last one it ever sent — that goes into the status frame as a solution
// quality it has no current evidence for.
func TestStaleGGAIsNotAttached(t *testing.T) {
	const (
		gga     = "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n"
		rmc     = "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n"
		noFix   = "$GPRMC,123519,V,,,,,,,230326,,*3A\r\n" // a sentence of ours, carrying nothing
		ignored = "$GPGSV,3,1,11,03,03,111,00,04,15,270,00,06,01,010,00,13,06,292,00*74\r\n"
		want    = 8
	)
	var r Reader
	feed(t, &r, gga)
	fix, ok := feed(t, &r, rmc)
	if !ok || fix.SatCount != want {
		t.Fatalf("a fix right after its GGA has %d satellites, want %d", fix.SatCount, want)
	}

	// The sentences this package does not read must not age a GGA. A
	// multi-constellation receiver sends fifteen or more of them between a
	// GGA and the RMC it belongs to, and counting those made every fix
	// outdoors report no satellites and no accuracy.
	var multi Reader
	feed(t, &multi, gga)
	for range 20 {
		feed(t, &multi, ignored)
	}
	fix, ok = feed(t, &multi, rmc)
	if !ok {
		t.Fatal("the RMC after a burst of GSVs produced no fix")
	}
	if fix.SatCount != want {
		t.Errorf("twenty ignored sentences aged the GGA: sats %d, want %d", fix.SatCount, want)
	}

	// Sentences this package does read age it, because they are what the
	// window is measured in.
	for range ggaStaleAfter + 1 {
		feed(t, &r, noFix)
	}
	fix, ok = feed(t, &r, rmc)
	if !ok {
		t.Fatal("the later RMC produced no fix")
	}
	if fix.SatCount != 0 || fix.AccuracyM != -1 || fix.AltitudeM != -500 {
		t.Errorf("a stale GGA was still attached: sats %d, accuracy %d, altitude %d",
			fix.SatCount, fix.AccuracyM, fix.AltitudeM)
	}
}

// TestCountersPartitionTheStream: every sentence is counted once, as good
// or as bad, never as both. Bad's whole use as a diagnostic is that a
// mis-clocked receiver shows as a rising Bad with Sentences standing still.
func TestCountersPartitionTheStream(t *testing.T) {
	var r Reader
	feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n")       // good
	feed(t, &r, "$GPGGA,123519,4807.038*45\r\n")                                            // too short to be a GGA
	feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,003.1,W*66\r\n") // bad checksum
	if got, want := r.Sentences(), uint64(1); got != want {
		t.Errorf("Sentences() = %d, want %d", got, want)
	}
	if got, want := r.Bad(), uint64(2); got != want {
		t.Errorf("Bad() = %d, want %d", got, want)
	}
}

// TestGGAWithNoFixVetoesTheRMC: quality 0 is the receiver saying it has no
// solution. An RMC beside it claiming its position is valid is not a second
// opinion — it is the same receiver disagreeing with itself, and on the
// bench that "valid" position was 150 km away with no satellites behind it,
// while a Totem on the same desk had a 2 m fix.
//
// This used to be the weaker rule: the GGA's figures were left off and the
// position still went out. A position with no satellites behind it is not
// one to put on the air at all.
func TestGGAWithNoFixVetoesTheRMC(t *testing.T) {
	var r Reader
	// Quality 0, but eight satellites and a good dilution in the fields
	// after it — which is exactly the shape that makes this worth checking.
	feed(t, &r, "$GPGGA,123519,4807.038,N,01131.000,E,0,08,0.9,545.4,M,46.9,M,,*46\r\n")
	if fix, ok := feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n"); ok {
		t.Errorf("an RMC was believed over its own receiver's GGA saying it has no fix: %+v", fix)
	}

	// And once the GGA reports a solution again, the RMC is believed and
	// carries its figures.
	feed(t, &r, "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n")
	fix, ok := feed(t, &r, "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18\r\n")
	if !ok {
		t.Fatal("a good GGA and RMC together produced no fix")
	}
	if fix.SatCount != 8 {
		t.Errorf("satellites %d, want 8 from the good GGA", fix.SatCount)
	}
	// And the sentence itself was well formed, so it counts as read.
	if r.Bad() != 0 {
		t.Errorf("%d bad sentences; a GGA with no fix is still a GGA", r.Bad())
	}
}

// TestNothingAfterTheChecksum: a line whose tail has been corrupted must not
// pass on the strength of a checksum covering less than the line holds.
func TestNothingAfterTheChecksum(t *testing.T) {
	const good = "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,,*18"
	var r Reader
	if _, ok := feed(t, &r, good+"\r\n"); !ok {
		t.Fatal("the sentence itself was refused")
	}
	for _, tail := range []string{"junk", "0", " "} {
		var r2 Reader
		if _, ok := feed(t, &r2, good+tail+"\r\n"); ok {
			t.Errorf("a sentence with %q after its checksum was accepted", tail)
		}
		if r2.Bad() == 0 {
			t.Errorf("a sentence with %q after its checksum was not counted as bad", tail)
		}
	}
}

// TestAbsurdSpeedPinsRatherThanWraps: the clamp has to happen before the
// conversion to an integer. Converting 1e20 to an int first is undefined —
// on a 64-bit host it lands on the most negative integer, and clamping that
// to zero turns the fastest reading a receiver ever gave into a standstill.
func TestAbsurdSpeedPinsRatherThanWraps(t *testing.T) {
	var r Reader
	fix, ok := feed(t, &r,
		"$GPRMC,123519,A,4807.038,N,01131.000,E,99999999999999999999,084.4,230326,,*32\r\n")
	if !ok {
		t.Fatal("a well-formed sentence with an absurd speed was refused")
	}
	if fix.SpeedKPH != 127 {
		t.Errorf("speed %d, want it pinned at 127", fix.SpeedKPH)
	}
}

func TestClampFloat(t *testing.T) {
	for _, tt := range []struct {
		in, want float64
	}{
		{5, 5}, {-5, 0}, {1e20, 127}, {-1e20, 0}, {math.NaN(), 0},
	} {
		if got := clampFloat(tt.in, 0, 127); got != tt.want {
			t.Errorf("clampFloat(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
