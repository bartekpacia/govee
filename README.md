# govee

Control Govee H6006 smart bulbs over Bluetooth LE,
locally, without the Govee app or cloud.

```
govee scan                     # list nearby Govee devices
govee on                       # all lamps
govee off table
govee color red                # names: red, green, blue, orange, yellow, purple, pink, cyan, white
govee color ff8000 drawer      # or RRGGBB hex
govee raw 330101 table         # raw packet; the checksum is added for you
govee --dry-run color red      # print packets, don't send
govee -v on                    # log decrypted protocol traffic
```

A LAMP is `table`, `drawer`, `all`, or a BLE MAC address.
Without LAMP arguments, a command applies to all lamps.
Lamp names are hard-coded in `knownLamps` in `govee.go`.

On Linux it talks to BlueZ over D-Bus, so it doesn't need root.
Only one BLE client can be connected to a bulb at a time:
close the Govee app on your phone first.

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

Brightness and color temperature are not verified yet;
use `govee raw` to experiment.

## Tests

```
go test ./...
```

The tests are black-box:
they build the binary and run it with `--dry-run`,
so they don't need Bluetooth or lamps.
