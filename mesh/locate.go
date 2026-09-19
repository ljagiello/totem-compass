package mesh

// LocateFrameLen is len(ENOW_MESH_BUFF).
const LocateFrameLen = 45

// Mesh relay constants from project_data and gen_mesh_msg's defaults.
const (
	DefaultMaxHops      = 99
	DefaultMinRSSI      = -127
	DefaultRelayMinDist = 10  // MESH_RELAY_MIN_DIST, meters
	LocateLifetimeSec   = 120 // gen_mesh_msg: expiry = rtc.unix() + 120
)

// Locate is the (2,0) mesh frame from Messages.gen_mesh_msg. A Totem
// broadcasts one with ReplyRequested set to find bonded peers it has not
// heard from directly; a peer that hears it answers with ReplyRequested
// clear, and every Totem in range relays both, flooding the mesh.
type Locate struct {
	Origin       MAC     // the Totem that built the frame
	Lat, Lon     float32 // origin position, degrees
	PosAccuracyM int8
	SOS          bool
	// UID de-duplicates the flood: randint(1, 65534) per new frame, kept
	// by relays and by the one repeat of a reply.
	UID            uint16
	ReplyRequested bool // the firmware calls this is_ack
	// MinRSSI and MaxRSSI bound the RSSI at which a receiver acts on the
	// frame. 5.0.3 always sends -127 and 0.
	MinRSSI, MaxRSSI int8
	// MinDistM and MaxDistM are always -1 and nothing reads them.
	MinDistM, MaxDistM int16
	Hops               uint8 // 0 at the origin, +1 per relay
	MaxHops            uint8
	Expiry             int32 // unix seconds; relays drop the frame after it
	// LastHopLat and LastHopLon are the position of whoever sent this copy:
	// each relay overwrites them with its own.
	LastHopLat, LastHopLon float32
	// RelayMinDistM: a Totem closer than this to the last hop does not
	// relay.
	RelayMinDistM int16
}

// locateWire is EXTENDED[(2,0)][45], '<BB6BffbbHbbbhhBBiffh'.
type locateWire struct {
	Category, Command      uint8
	Origin                 MAC
	Lat, Lon               float32
	PAcc, SOS              int8
	UID                    uint16
	ReplyRequested         int8
	MinRSSI, MaxRSSI       int8
	MinDist, MaxDist       int16
	Hops, MaxHops          uint8
	Expiry                 int32
	LastHopLat, LastHopLon float32
	RelayMinDist           int16
}

func (Locate) isMessage() {}

// MarshalBinary encodes the 45-byte frame.
func (l Locate) MarshalBinary() ([]byte, error) {
	return appendWire(make([]byte, 0, LocateFrameLen), locateWire{
		Category: CatMesh, Origin: l.Origin, Lat: l.Lat, Lon: l.Lon,
		PAcc: l.PosAccuracyM, SOS: flag(l.SOS), UID: l.UID, ReplyRequested: flag(l.ReplyRequested),
		MinRSSI: l.MinRSSI, MaxRSSI: l.MaxRSSI, MinDist: l.MinDistM, MaxDist: l.MaxDistM,
		Hops: l.Hops, MaxHops: l.MaxHops, Expiry: l.Expiry,
		LastHopLat: l.LastHopLat, LastHopLon: l.LastHopLon, RelayMinDist: l.RelayMinDistM,
	})
}

func parseLocate(b []byte) (Message, error) {
	var w locateWire
	if err := readAt(b, 2, &w); err != nil {
		return nil, err
	}
	return Locate{
		Origin: w.Origin, Lat: w.Lat, Lon: w.Lon, PosAccuracyM: w.PAcc, SOS: w.SOS != 0,
		UID: w.UID, ReplyRequested: w.ReplyRequested != 0,
		MinRSSI: w.MinRSSI, MaxRSSI: w.MaxRSSI, MinDistM: w.MinDist, MaxDistM: w.MaxDist,
		Hops: w.Hops, MaxHops: w.MaxHops, Expiry: w.Expiry,
		LastHopLat: w.LastHopLat, LastHopLon: w.LastHopLon, RelayMinDistM: w.RelayMinDist,
	}, nil
}
