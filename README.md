# Totem Compass firmware — reverse engineering

Independent, unofficial reverse-engineering notes for the **Totem Compass** ESP32
firmware (**v5.0.3**; v5.0.2 was analyzed first, see
[What changed in 5.0.3](docs/firmware/changes-5.0.3.mdx)), produced by static analysis of the
publicly downloadable firmware images and the companion mobile app, plus a BLE client tested
against the author's own device. Not affiliated with or endorsed by Totem Labs.

## Key findings

- The device is an **ESP32** (Xtensa LX6) running **MicroPython v1.25.0** on **ESP-IDF v5.4**.
- Totem's application is **Python**, frozen into the image as bytecode across **94 frozen modules** in v5.0.3 (96 in v5.0.2), plus 2 non-frozen on-flash `.py` files.
- Three radios share the 2.4 GHz band: **BLE** (phone app), **ESP-NOW** ("Unity Mesh",
  Totem-to-Totem), and **WiFi** (station-mode OTA; the hotspot path is sunset).
- Application messages use a shared `(cat_id, cmd_id)` model with a common chunking layer.

## Documentation

Full docs live in [`docs/`](docs/) as a [Mintlify](https://mintlify.com) site.

```bash
cd docs
npm i -g mint    # or: npx mint@latest
mint dev         # preview at http://localhost:3000
```

Start at [`docs/index.mdx`](docs/index.mdx). The 2.4 GHz protocol capture — with a
completeness matrix — is under [`docs/protocols/`](docs/protocols/).

## Go client: `totemctl`

A Bluetooth LE client that speaks the same protocol as the official app: the
ConnStatus-Ready handshake, Live/Static/Peer records, and the half-duplex TX
handoff. No pairing or keys are needed.

```bash
go install github.com/ljagiello/totem-compass/cmd/totemctl@latest

totemctl scan                     # find Totems (double-press the power button first)
totemctl info                     # name, firmware, battery, position, settings
totemctl watch                    # stream live data and peer updates
totemctl peers                    # bonded Totems and points of interest
totemctl name "Base Camp"         # rename
totemctl compass --lock on --north off
totemctl power eco                # or: normal
totemctl wifi scan                # networks the Totem can see
totemctl wifi set <ssid> <pass>   # network used for firmware updates
totemctl peer color <mac> hot_pink
totemctl poi add --name "Main Stage" --lat 50.0671 --lon 19.9124 --sticky
totemctl location --lat 50.0671 --lon 19.9124  # feed a phone-style GNSS fix + clock
```

Pick a device with `-d <name or address substring>`, get machine-readable
output with `--json`, and log every frame with `--trace`. `totemctl help
<command>` lists each command's flags. Every global flag can also be set as a
`TOTEM_<FLAG>` environment variable (`TOTEM_DEVICE`, `TOTEM_HALF_DUPLEX`, …) or
in a config file with the long flag names as keys (`device: Base Camp`). The
default location is `totemctl/config.yaml` under the user config directory
(`~/Library/Application Support` on macOS, `~/.config` on Linux), or pass
`--config`.

The Ready frame's schema id selects one of two device transmit loops. The
client defaults to the legacy full-duplex loop (schema 0: notifications plus
app-level acks), which works on every platform. The newer half-duplex loop
(`--half-duplex`) sends indications and drops a record unless it is confirmed
within 500 ms; CoreBluetooth subscribes these characteristics for
notifications only, so that mode stalls on macOS.

Verified on hardware from macOS: `scan` and `info` on firmware 4.1.3 and 5.0.3; `watch` and
`compass --lock on|off` round-trips on 4.1.3. The other commands are built from the
v5.0.2/v5.0.3 bytecode (their handlers are identical in both) and covered by unit tests
only.

| Package | Contents |
| --- | --- |
| [`protocol/`](protocol/) | frame builders and decoders, no BLE dependency; tested against vectors built with the firmware's own `struct` formats |
| [`client/`](client/) | scanning, connecting, subscriptions, acks and TX-window handling on [`tinygo.org/x/bluetooth`](https://github.com/tinygo-org/bluetooth) (tested on macOS; the library also supports Linux/BlueZ and Windows) |
| [`cmd/totemctl/`](cmd/totemctl/) | the CLI |

## Scope & ethics

The firmware analysis is static and no service was attacked; the releases API and firmware
objects are served publicly. The only code run against hardware is `totemctl`, over BLE, on
the author's own Totem. See [`docs/reference/methodology.mdx`](docs/reference/methodology.mdx).
Use it only with devices you own. The mesh and demi-god paths can affect other people's
Totems nearby, so `totemctl` does not send those commands.

## License

Licensed under the [Apache License 2.0](LICENSE).
