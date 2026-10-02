package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rc4"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"net"
	"slices"
	"strings"
)

// Govee's BLE protocol (v1 encryption), as used by the H6006 bulb.
//
// Every message is a 20-byte packet whose last byte is the XOR of the others.
// Before sending commands, the client performs a two-step handshake
// encrypted with a pre-shared key; the bulb replies with a session key
// that encrypts all further packets.
// Encryption: AES-128-ECB on bytes 0-15, RC4 on bytes 16-19, same key for both.
//
// Source: openHAB's Govee binding (openhab/openhab-addons#20976).

const packetLen = 20

// psk is the pre-shared handshake key. It is the same for every device.
var psk = []byte("MakingLifeSmarte")

// knownLamps maps lamp names to their BLE addresses.
var knownLamps = map[string]string{
	"table":  "5C:E7:53:C7:2D:2F",
	"drawer": "5C:E7:53:C8:3A:37",
}

// namedColors maps color names to RGB hex.
// Orange is ff8000 rather than CSS's ffa500, which looks yellow on these bulbs.
var namedColors = map[string]string{
	"red":    "ff0000",
	"green":  "00ff00",
	"blue":   "0000ff",
	"orange": "ff8000",
	"yellow": "ffff00",
	"purple": "8000ff",
	"pink":   "ff0080",
	"cyan":   "00ffff",
	"white":  "ffffff",
}

// packet builds a packet from payload, which must be at most 19 bytes.
func packet(payload ...byte) []byte {
	p := make([]byte, packetLen)
	copy(p, payload)
	for _, b := range p[:packetLen-1] {
		p[packetLen-1] ^= b
	}
	return p
}

func powerPacket(on bool) []byte {
	if on {
		return packet(0x33, 0x01, 0x01)
	}
	return packet(0x33, 0x01, 0x00)
}

// colorPacket sets an RGB color. Mode 0x0d works on the H6006;
// mode 0x02 (used by older bulbs) is acknowledged but ignored.
func colorPacket(rgb [3]byte) []byte {
	return packet(0x33, 0x05, 0x0d, rgb[0], rgb[1], rgb[2])
}

// colorPayload parses a color and brightness into a color packet.
// percent is 1 to 100; the bulbs dim by using a darker color.
func colorPayload(color string, percent int) ([]byte, error) {
	rgb, err := parseColor(color)
	if err != nil {
		return nil, err
	}
	if percent < 1 || percent > 100 {
		return nil, fmt.Errorf("invalid brightness %d (want 1 to 100; use off to turn lamps off)", percent)
	}
	return colorPacket(dim(rgb, percent)), nil
}

func handshakePacket(step byte) []byte {
	return packet(0xe7, step)
}

func encrypt(key, p []byte) []byte { return crypt(key, p, cipher.Block.Encrypt) }
func decrypt(key, p []byte) []byte { return crypt(key, p, cipher.Block.Decrypt) }

func crypt(key, p []byte, aesFunc func(b cipher.Block, dst, src []byte)) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // keys are always 16 bytes
	}
	stream, err := rc4.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, packetLen)
	aesFunc(block, out[:16], p[:16])
	stream.XORKeyStream(out[16:], p[16:packetLen])
	return out
}

type lamp struct {
	name string
	addr string // upper-case MAC address
}

// resolveLamps turns lamp names, "all", and MAC addresses into a list
// without duplicates. No arguments means all known lamps.
func resolveLamps(args []string) ([]lamp, error) {
	if len(args) == 0 {
		args = []string{"all"}
	}
	var lamps []lamp
	add := func(l lamp) {
		if !slices.Contains(lamps, l) {
			lamps = append(lamps, l)
		}
	}
	for _, arg := range args {
		if arg == "all" {
			for _, name := range slices.Sorted(maps.Keys(knownLamps)) {
				add(lamp{name, knownLamps[name]})
			}
			continue
		}
		if addr, ok := knownLamps[arg]; ok {
			add(lamp{arg, addr})
			continue
		}
		hw, err := net.ParseMAC(arg)
		if err != nil || len(hw) != 6 {
			names := strings.Join(slices.Sorted(maps.Keys(knownLamps)), ", ")
			return nil, fmt.Errorf("unknown lamp %q (want %s, all, or a MAC address)", arg, names)
		}
		addr := strings.ToUpper(hw.String())
		add(lamp{lampName(addr), addr})
	}
	return lamps, nil
}

// lampName returns the known name of addr, or addr itself.
func lampName(addr string) string {
	for name, a := range knownLamps {
		if a == addr {
			return name
		}
	}
	return addr
}

// parseColor accepts a color name or RRGGBB hex, optionally prefixed with #.
func parseColor(s string) ([3]byte, error) {
	h := strings.ToLower(s)
	if named, ok := namedColors[h]; ok {
		h = named
	}
	b, err := hex.DecodeString(strings.TrimPrefix(h, "#"))
	if err != nil || len(b) != 3 {
		names := strings.Join(slices.Sorted(maps.Keys(namedColors)), ", ")
		return [3]byte{}, fmt.Errorf("invalid color %q (want RRGGBB hex or one of: %s)", s, names)
	}
	return [3]byte(b), nil
}

// dim scales a color to percent (1 to 100) of its brightness.
// The bulbs have no separate brightness setting; darker colors are dimmer.
func dim(rgb [3]byte, percent int) [3]byte {
	for i, c := range rgb {
		rgb[i] = byte(math.Round(float64(c) * float64(percent) / 100))
	}
	return rgb
}

// parseRaw parses a hex command payload of 1 to 19 bytes.
func parseRaw(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) == 0 || len(b) > packetLen-1 {
		return nil, fmt.Errorf("invalid raw command %q (want 1 to %d bytes of hex, e.g. 330101)", s, packetLen-1)
	}
	return b, nil
}
