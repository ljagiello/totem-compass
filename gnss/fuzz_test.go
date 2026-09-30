package gnss

import (
	"math"
	"testing"
	"time"
)

// FuzzReader feeds the reader arbitrary bytes, which is what a serial line
// from a receiver is: nobody on this side chose them, a cable can drop or
// invent a character, and the module can be replaced with one that talks
// about constellations this package has never heard of.
//
// The reader is not asked to make sense of rubbish. It is asked never to
// panic on it, and never to hand back a fix that is not a place: a latitude
// past the poles, a longitude past the antimeridian, a speed that wraps
// negative or a clock outside the years these receivers were built in would
// each go straight into a status frame and out over the air to every peer.
func FuzzReader(f *testing.F) {
	// The two sentences the board's own receiver sends with no sky in
	// sight, and a fix, so the corpus starts from real traffic.
	f.Add([]byte("$GNRMC,001655.00,V,,,,,,,290926,,,N,V*18\r\n$GNGGA,001655.00,,,,,0,00,99.99,,,,,,*7F\r\n"))
	f.Add([]byte("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n" +
		"$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230326,003.1,W*63\r\n"))
	f.Add([]byte("$GPRMC,123519,A,3352.000,S,15112.000,W,000.0,000.0,230326,,*18\r\n"))
	f.Add([]byte("$GPGSV,3,1,11,03,03,111,00,04,15,270,00,06,01,010,00,13,06,292,00*74\r\n"))
	// Shapes worth starting from: an empty sentence, a lone dollar, a
	// sentence with nothing but separators, and one much too long.
	f.Add([]byte("$*00\r\n"))
	f.Add([]byte("$"))
	f.Add([]byte("$,,,,,,,,,,,,*00\r\n"))
	f.Add([]byte("$GPRMC" + string(make([]byte, 400))))
	// Fields that parse as numbers but are not numbers a receiver means.
	// The fuzzer did not synthesize these on its own — a matching checksum
	// over the exact letters N, a, N is a lot to ask of random mutation —
	// so they are seeded, and the invariant they break is the one about
	// never handing back a fix that is not a place.
	f.Add([]byte("$GPRMC,123519,A,NaN12.0,N,01131.000,E,022.4,084.4,230326,003.1,W*01\r\n"))
	f.Add([]byte("$GPRMC,123519,A,4807.038,N,0x1p112.0,E,022.4,084.4,230326,,*11\r\n"))
	f.Add([]byte("$GPGGA,123519,4807.038,N,01131.000,E,1,08,NaN,545.4,M,46.9,M,,*01\r\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		var r Reader
		for _, b := range data {
			fix, ok := r.Feed(b)
			if !ok {
				continue
			}
			checkFix(t, fix)
		}
		// The block form has to agree with the byte form about what is
		// acceptable, since the driver uses it and only it.
		var r2 Reader
		if fix, ok := r2.FeedAll(data); ok {
			checkFix(t, fix)
		}
		// Counting must not overflow into nonsense either: every sentence
		// is one or the other, never both.
		if r.Sentences() > uint64(len(data)) || r.Bad() > uint64(len(data)) {
			t.Fatalf("counted %d sentences and %d bad from %d bytes",
				r.Sentences(), r.Bad(), len(data))
		}
	})
}

// checkFix holds a fix to what a Totem can put in a frame and what the
// Earth allows.
func checkFix(t *testing.T, fix Fix) {
	t.Helper()
	if math.IsNaN(float64(fix.Lat)) || math.IsNaN(float64(fix.Lon)) {
		t.Fatalf("fix at NaN: %+v", fix)
	}
	if fix.Lat < -90 || fix.Lat > 90 {
		t.Fatalf("latitude %v is off the Earth", fix.Lat)
	}
	if fix.Lon < -180 || fix.Lon > 180 {
		t.Fatalf("longitude %v is past the antimeridian", fix.Lon)
	}
	if fix.AccuracyM < -1 {
		t.Fatalf("accuracy %d: -1 is the only value below zero that means anything", fix.AccuracyM)
	}
	if fix.AltitudeM < -500 {
		t.Fatalf("altitude %d is below the unknown marker", fix.AltitudeM)
	}
	if fix.SpeedKPH < 0 {
		t.Fatalf("speed %d is negative", fix.SpeedKPH)
	}
	if fix.SatCount < 0 {
		t.Fatalf("satellite count %d is negative", fix.SatCount)
	}
	if fix.CourseDeg < -1 || fix.CourseDeg > 359 {
		t.Fatalf("course %d is not a bearing, and -1 is how this says it has none", fix.CourseDeg)
	}
	if !fix.Time.IsZero() {
		// The two-digit year is read as this century, so anything outside
		// it means the date arithmetic went wrong rather than the
		// receiver being old.
		if y := fix.Time.Year(); y < 2000 || y > 2099 {
			t.Fatalf("clock in year %d, from a two-digit year: %v", y, fix.Time)
		}
		if fix.Time.Location() != time.UTC {
			t.Fatalf("clock is not in UTC: %v", fix.Time)
		}
	}
}
