package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

var (
	serviceUUID = mustParseUUID("00010203-0405-0607-0809-0a0b0c0d1910")
	notifyUUID  = mustParseUUID("00010203-0405-0607-0809-0a0b0c0d2b10") // bulb -> us
	writeUUID   = mustParseUUID("00010203-0405-0607-0809-0a0b0c0d2b11") // us -> bulb
)

const (
	connectAttempts = 3
	replyTimeout    = 5 * time.Second
)

func mustParseUUID(s string) bluetooth.UUID {
	u, err := bluetooth.ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

type logFunc func(format string, args ...any)

// sendAll sends payload to every lamp and returns one error (or nil) per lamp.
// It connects to all lamps first and then sends to them together,
// so that their changes happen at the same moment.
func sendAll(ctx context.Context, lamps []lamp, payload []byte, logf logFunc) ([]error, error) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable bluetooth adapter: %w", err)
	}

	found, err := scanFor(ctx, adapter, lamps)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	results := make([]error, len(lamps))
	sessions := make([]*session, len(lamps))
	for i, l := range lamps {
		addr, ok := found[l.addr]
		if !ok {
			results[i] = errors.New("not found (out of range, or is a phone connected to it?)")
			continue
		}
		s, err := openSession(ctx, adapter, addr, logf)
		if err != nil {
			results[i] = err
			continue
		}
		defer s.close()
		sessions[i] = s
	}

	var wg sync.WaitGroup
	for i, s := range sessions {
		if s == nil {
			continue
		}
		wg.Go(func() {
			_, results[i] = s.exchange(ctx, s.key, payload)
		})
	}
	wg.Wait()
	return results, nil
}

// scanFor scans until all lamps are seen or ctx is done.
func scanFor(ctx context.Context, adapter *bluetooth.Adapter, lamps []lamp) (map[string]bluetooth.Address, error) {
	found := make(map[string]bluetooth.Address)
	stop := context.AfterFunc(ctx, func() { adapter.StopScan() })
	defer stop()
	err := adapter.Scan(func(a *bluetooth.Adapter, r bluetooth.ScanResult) {
		addr := strings.ToUpper(r.Address.String())
		if !slices.ContainsFunc(lamps, func(l lamp) bool { return l.addr == addr }) {
			return
		}
		found[addr] = r.Address
		if len(found) == len(lamps) {
			a.StopScan()
		}
	})
	return found, err
}

type device struct {
	addr string
	name string
	rssi int16
}

// discover lists nearby Govee devices seen within d.
func discover(ctx context.Context, d time.Duration) ([]device, error) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable bluetooth adapter: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { adapter.StopScan() })
	defer stop()

	seen := make(map[string]device)
	err := adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
		name := r.LocalName()
		if !isGovee(name) {
			return
		}
		addr := strings.ToUpper(r.Address.String())
		seen[addr] = device{addr: addr, name: name, rssi: r.RSSI}
	})
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	devices := slices.Collect(maps.Values(seen))
	slices.SortFunc(devices, func(a, b device) int { return cmp.Compare(b.rssi, a.rssi) })
	return devices, nil
}

func isGovee(name string) bool {
	for _, prefix := range []string{"GVH", "Govee_", "ihoment_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// session is an open, handshaken connection to one lamp.
type session struct {
	device bluetooth.Device
	write  bluetooth.DeviceCharacteristic
	notify chan []byte
	key    []byte
	logf   logFunc
}

func openSession(ctx context.Context, adapter *bluetooth.Adapter, addr bluetooth.Address, logf logFunc) (*session, error) {
	var dev bluetooth.Device
	var err error
	for attempt := range connectAttempts {
		if attempt > 0 {
			logf("%s: connect failed (%v), retrying", addr, err)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		dev, err = adapter.Connect(addr, bluetooth.ConnectionParams{})
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	s := &session{device: dev, notify: make(chan []byte, 4), logf: logf}
	if err := s.setup(); err != nil {
		dev.Disconnect()
		return nil, err
	}
	if err := s.handshake(ctx); err != nil {
		dev.Disconnect()
		return nil, err
	}
	return s, nil
}

func (s *session) setup() error {
	services, err := s.device.DiscoverServices([]bluetooth.UUID{serviceUUID})
	if err != nil {
		return fmt.Errorf("discover service: %w", err)
	}
	if len(services) == 0 {
		return errors.New("Govee service not found")
	}
	chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{notifyUUID, writeUUID})
	if err != nil {
		return fmt.Errorf("discover characteristics: %w", err)
	}
	var notify *bluetooth.DeviceCharacteristic
	for i, c := range chars {
		switch c.UUID() {
		case notifyUUID:
			notify = &chars[i]
		case writeUUID:
			s.write = c
		}
	}
	if notify == nil {
		return errors.New("notify characteristic not found")
	}
	return notify.EnableNotifications(func(b []byte) {
		select {
		case s.notify <- bytes.Clone(b):
		default: // nobody is waiting; drop it
		}
	})
}

func (s *session) handshake(ctx context.Context) error {
	reply, err := s.exchange(ctx, psk, handshakePacket(1))
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	s.key = bytes.Clone(reply[2:18])
	if _, err := s.exchange(ctx, psk, handshakePacket(2)); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

// exchange sends one packet and waits for the reply, which must echo
// the packet's first two bytes.
func (s *session) exchange(ctx context.Context, key, p []byte) ([]byte, error) {
	s.logf("%s: tx % x", s.device.Address, p)
	if _, err := s.write.WriteWithoutResponse(encrypt(key, p)); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	select {
	case b := <-s.notify:
		if len(b) != packetLen {
			return nil, fmt.Errorf("unexpected %d-byte reply", len(b))
		}
		reply := decrypt(key, b)
		s.logf("%s: rx % x", s.device.Address, reply)
		if reply[0] != p[0] || reply[1] != p[1] {
			return nil, fmt.Errorf("unexpected reply % x", reply)
		}
		return reply, nil
	case <-time.After(replyTimeout):
		return nil, errors.New("no reply from lamp")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *session) close() {
	s.device.Disconnect()
}
