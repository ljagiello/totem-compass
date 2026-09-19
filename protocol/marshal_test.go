package protocol

import (
	"bytes"
	"encoding"
	"reflect"
	"testing"
)

// Re-encoding a decoded record must reproduce the original bytes exactly:
// real device frames for Live/Static (whose reserved bytes the golden
// vectors don't model), golden vectors for the rest.
func TestMarshalReproducesFrames(t *testing.T) {
	for _, name := range []string{"live 5.0.3", "static 5.0.3", "peer_ping", "peer_sync", "wifi"} {
		t.Run(name, func(t *testing.T) {
			want := mustHex(t, name)
			m, err := Parse(Data, want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("re-encoded\n got % x\nwant % x", got, want)
			}
		})
	}
}

// Parse(Marshal(x)) == x for every decoded field, including the golden
// vectors built with other reserved-byte values.
func TestMarshalRoundTrip(t *testing.T) {
	for _, name := range []string{"live", "static", "peer_ping", "live 4.1.3", "static 4.1.3"} {
		t.Run(name, func(t *testing.T) {
			m, err := Parse(Data, mustHex(t, name))
			if err != nil {
				t.Fatal(err)
			}
			b, err := m.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			again, err := Parse(Data, b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(again, m) {
				t.Errorf("round trip changed the record\n got %+v\nwant %+v", again, m)
			}
		})
	}
}

// The fields named from app 2.3.0's parsers sit at the frame offsets the app
// reads them from, and survive a round trip.
func TestFieldsAtAppOffsets(t *testing.T) {
	check := func(name string, m encoding.BinaryMarshaler, off int, want ...byte) {
		t.Helper()
		b, err := m.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if got := b[off : off+len(want)]; !bytes.Equal(got, want) {
			t.Errorf("%s: bytes at %d = % x, want % x", name, off, got, want)
		}
		again, err := Parse(Data, b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(again, m) {
			t.Errorf("%s: round trip\n got %+v\nwant %+v", name, again, m)
		}
	}
	// useStaticDataParser: settings flags at 20 (bit 3 bond chat), capability
	// flags at 21 (bit 0 isHalfDuplex).
	check("static", StaticData{Version: "5.0.3", BondChat: true, HalfDuplex: true}, 20, 0x08, 0x01)
	// usePeerDataParser: flags at 22 (bit 5 isIdle), dtim as UInt16LE at 26.
	check("peer ping", PeerPing{Idle: true, DTIM: 0x1234}, 22, 0x20)
	check("peer ping", PeerPing{DTIM: 0x1234}, 26, 0x34, 0x12)
	// useLiveDataParser: flags at 68 (bit 4 isLowBatt).
	check("live", LiveData{LowBattery: true}, 68, 0x10)
}

func TestMarshalRejectsBadInput(t *testing.T) {
	if _, err := (StaticData{Version: "five"}).MarshalBinary(); err == nil {
		t.Error("static data with an unparsable version encoded")
	}
	if _, err := (PeerPing{Name: string(make([]byte, 200))}).MarshalBinary(); err == nil {
		t.Error("peer ping with a 200-byte name encoded")
	}
}
