package clienttest

import (
	"testing"

	"github.com/ljagiello/totem-compass/protocol"
)

// FuzzWrite replays arbitrary app writes against the simulated firmware:
// each fuzz input is a sequence of [length, channel, frame...] entries. No
// write may panic, and whatever state the writes leave must still encode,
// since the send loop panics on a record it cannot encode.
func FuzzWrite(f *testing.F) {
	frame := func(ch protocol.Channel, fr []byte) []byte { return append([]byte{byte(len(fr)), byte(ch)}, fr...) }
	join := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}
	ready := frame(protocol.ConnStatus, protocol.Ready(protocol.SchemaLegacy).Bytes)
	name, _ := protocol.SetName("Base Camp")
	wifi, _ := protocol.SaveWiFi("home", "pw")
	poi, _ := protocol.AddPOI(protocol.POI{ID: protocol.MAC{2, 1}, Name: "Stage", Lat: 1, Lon: 2})
	details, _ := protocol.RequestPeerDetails(protocol.MAC{2, 1})
	for _, fr := range []protocol.Frame{
		name, wifi, poi, details, protocol.RequestStaticData(), protocol.RequestPeerSync(), protocol.ScanWiFi(),
		protocol.SetCompassPrefs(protocol.CompassPrefs{CompassLock: true, PowerMode: protocol.PowerEco}),
		protocol.UpdatePeer(protocol.PeerUpdate{MAC: protocol.MAC{2, 1}, Hidden: true}),
		protocol.SendPhoneFix(protocol.PhoneFix{Lat: 1, Lon: 2}), protocol.DisconnectRequest(),
	} {
		f.Add(join(ready, frame(fr.Channel, fr.Bytes)))
	}
	f.Add(join(ready, frame(poi.Channel, poi.Bytes), frame(details.Channel, details.Bytes)))
	f.Fuzz(func(t *testing.T, data []byte) {
		tt := New()
		tt.running = true // drive the send loop by hand
		for len(data) >= 2 {
			n, ch := int(data[0]), protocol.Channel(data[1]&1)
			data = data[2:]
			n = min(n, len(data))
			fr := data[:n]
			data = data[n:]
			_ = tt.Write(ch, fr) // errors are fine; panics are not
			tt.mu.Lock()
			for range 8 {
				rec := tt.nextRecord()
				if rec == nil {
					continue
				}
				if _, err := rec.MarshalBinary(); err != nil {
					tt.mu.Unlock()
					t.Fatalf("after writing % x the simulator cannot encode %T: %v", fr, rec, err)
				}
			}
			tt.mu.Unlock()
		}
	})
}
