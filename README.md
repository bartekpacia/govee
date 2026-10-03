# govee

Control Govee H6006 smart bulbs over Bluetooth LE,
locally, without the Govee app or cloud.

```
go install github.com/bartekpacia/govee@latest
```

Usage:

```
govee scan                     # list nearby Govee devices
govee on                       # all lamps
govee off table
govee color red                # names: red, green, blue, orange, yellow, purple, pink, cyan, white
govee color ff8000 drawer      # or RRGGBB hex
govee color red --brightness 30   # percent; dims by scaling the color
govee raw 330101 table         # raw packet; the checksum is added for you
govee music                    # follow the audio playing on this machine (Ctrl-C to stop)
govee music --delay 200ms      # delay the lights to match a Bluetooth speaker's latency
govee --dry-run color red      # print packets, don't send
govee -v on                    # log decrypted protocol traffic
GOVEE_TOKEN=… govee mcp       # MCP server for Claude and other clients
```

A LAMP is `table`, `drawer`, `all`, or a BLE MAC address.
Without LAMP arguments, a command applies to all lamps.
Lamp names are hard-coded in `knownLamps` in `govee.go`.

On Linux it talks to BlueZ over D-Bus, so it doesn't need root.
Only one BLE client can be connected to a bulb at a time:
close the Govee app on your phone first.

## MCP server

`govee mcp` speaks the [Model Context Protocol](https://modelcontextprotocol.io)
over HTTP, so a client such as Claude can turn the lamps on and off
and set their color. The tools are `list_lamps`, `scan_lamps`,
`turn_on`, `turn_off`, and `set_color`.

The process has to be in Bluetooth range of the bulbs.
The machine you already run `govee` on is enough.
A Raspberry Pi is worth adding when you want a small box that stays
on in that room: a Pi Zero 2 W (Wi-Fi and Bluetooth, 64-bit) is the
cheap one that works, and a Pi 3, 4, or 5 does too.
The original Pi Zero and the Pico do not.

It listens on `127.0.0.1:8080` and rejects every request that
does not send `Authorization: Bearer TOKEN`.
The token is required, at least 26 characters long, and comes from
`--token` or `GOVEE_TOKEN`; `openssl rand -hex 16` makes a good one.
`--dry-run` works the same way as on the other commands:
tools print the packet they would send and do not touch Bluetooth.

Put a TLS reverse proxy (Caddy, nginx) in front before the server
is reachable from the internet; over plain HTTP the token travels in the clear.
Anyone with the token can change the lights, and nothing else.

Run it as a service, with the token in a file only its user can read
(`/etc/govee.env`, mode `600`):

```
GOVEE_TOKEN=paste-a-long-random-token
```

```
[Unit]
Description=Govee lamp MCP server
After=bluetooth.target

[Service]
ExecStart=/usr/local/bin/govee mcp
EnvironmentFile=/etc/govee.env
Restart=on-failure
User=pi
SupplementaryGroups=bluetooth

[Install]
WantedBy=multi-user.target
```

`User` has to be in the `bluetooth` group
(`sudo usermod -aG bluetooth pi`), because the program talks to
BlueZ over D-Bus and does not need root.
Build a binary for the Pi from a 64-bit machine with
`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o govee .`

Claude Code connects with the token in a header:

```
claude mcp add --transport http govee https://<your-host>/mcp \
  --header "Authorization: Bearer $GOVEE_TOKEN"
```

In claude.ai, add a custom connector for `https://<your-host>/mcp`
with "No sign-in", and add the request header `Authorization`
with the value `Bearer ` followed by the token.

`govee music` stays a local command. It listens to the audio
playing on the machine it runs on, which a Pi in the corner usually is not.

## Music mode

`govee music` records the default output's monitor
(a copy of everything being played) with `parec`,
and updates the lamps 20 times per second over open connections:

- On every beat (a sudden rise of the bass), the hue rotates by 137.5°,
  so consecutive colors are always clearly different.
- Brightness follows loudness relative to the recent peak:
  it jumps up immediately and fades out.
  Dimming works by scaling the RGB values.

The lamps visibly keep up with 20 updates per second
(measured with the hidden `govee bench` command).

## Protocol

Newer Govee firmware silently ignores plaintext commands.
Commands must be encrypted (Govee's "v1" scheme),
taken from [openHAB's Govee binding](https://github.com/openhab/openhab-addons/pull/20976):

- GATT service `00010203-0405-0607-0809-0a0b0c0d1910`:
  write to `…2b11`, replies arrive as notifications on `…2b10`.
- Every packet is 20 bytes; the last byte is the XOR of the other 19.
- Handshake: send `e7 01`, encrypted with the pre-shared key `MakingLifeSmarte`.
  The reply's bytes 2–17 are the session key.
  Then send `e7 02` (also with the pre-shared key).
- All further packets are encrypted with the session key.
- Encryption: AES-128-ECB on bytes 0–15, RC4 on bytes 16–19.
- The bulb acknowledges a command by echoing its first two bytes plus `00`.
  An acknowledgement does **not** prove the command had a visible effect.

Commands verified on the H6006:

| Command | Packet | Note |
|---|---|---|
| power | `33 01 01` / `33 01 00` | |
| color | `33 05 0d RR GG BB` | `33 05 02 RR GG BB` (older bulbs) is acknowledged but ignored |

Dimming works by scaling the color (`--brightness`).
A dedicated brightness command and color temperature are not verified yet;
use `govee raw` to experiment.

## Tests

```
go test ./...
```

The tests are black-box:
they build the binary and run it with `--dry-run`,
so they don't need Bluetooth or lamps.
