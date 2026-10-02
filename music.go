package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"time"

	"github.com/urfave/cli/v3"
)

// Audio format read from the input: 48 kHz, mono, signed 16-bit little-endian.
const sampleRate = 48000

// Tuning of the "pulse" effect.
const (
	bassCutoff    = 150.0 // Hz; below this counts as bass
	beatRatio     = 1.5   // a beat is bass this many times above its recent average
	minBeatGap    = 250 * time.Millisecond
	silence       = 0.001 // RMS below this (-60 dBFS) is silence
	goldenAngle   = 137.5 // hue step per beat, in degrees; keeps colors distinct
	minBrightness = 0.03
	gamma         = 2.2             // makes quiet parts darker, so pulses stand out
	peakHalfLife  = 5 * time.Second // automatic gain adapts this fast
	fadeHalfLife  = 150 * time.Millisecond
	maxFailures   = 5 // consecutive send errors before giving up on a lamp
)

// music makes lamps follow audio: the color changes on every beat
// and the brightness follows loudness.
func music(ctx context.Context, cmd *cli.Command) error {
	rate := cmd.Float("rate")
	if rate <= 0 || rate > 50 {
		return fmt.Errorf("invalid rate %g (want 0 < rate <= 50)", rate)
	}
	lamps, err := resolveLamps(cmd.StringArgs("lamp"))
	if err != nil {
		return err
	}
	audio, closeAudio, err := openAudio(ctx, cmd)
	if err != nil {
		return err
	}
	defer closeAudio()

	hop := int(sampleRate / rate) // samples per update
	a := newAnalyzer(rate)
	frames := make(chan [3]byte)
	readErr := make(chan error, 1)
	go func() {
		defer close(frames)
		readErr <- readFrames(audio, hop, func(samples []int16) {
			select {
			case frames <- a.next(samples):
			case <-ctx.Done():
			}
		})
	}()

	if cmd.Bool("dry-run") {
		for c := range frames {
			fmt.Fprintf(cmd.Root().Writer, "%x\n", c)
		}
		return <-readErr
	}

	connectCtx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()
	sessions, results, err := connectAll(connectCtx, lamps, logger(cmd))
	if err != nil {
		return err
	}
	defer closeAll(sessions)
	for i, l := range lamps {
		if results[i] != nil {
			fmt.Fprintf(cmd.Root().ErrWriter, "govee: skipping %s: %v\n", l.name, results[i])
		}
	}

	latest := make(chan [3]byte, 1)
	go delayFrames(frames, latest, int(math.Round(cmd.Duration("delay").Seconds()*rate)))
	if err := sendFrames(ctx, cmd, lamps, sessions, latest); err != nil {
		return err
	}
	if err := <-readErr; err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// openAudio opens --input, or records what this machine is playing.
func openAudio(ctx context.Context, cmd *cli.Command) (io.Reader, func(), error) {
	switch input := cmd.String("input"); input {
	case "":
		// The default sink's monitor is a copy of everything being played.
		rec := exec.CommandContext(ctx, "parec", "-d", "@DEFAULT_MONITOR@",
			"--raw", "--format=s16le", fmt.Sprintf("--rate=%d", sampleRate), "--channels=1", "--latency-msec=20")
		rec.Stderr = os.Stderr
		out, err := rec.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := rec.Start(); err != nil {
			return nil, nil, fmt.Errorf("start parec: %w", err)
		}
		return out, func() { rec.Process.Kill(); rec.Wait() }, nil
	case "-":
		return cmd.Root().Reader, func() {}, nil
	default:
		f, err := os.Open(input)
		if err != nil {
			return nil, nil, err
		}
		return f, func() { f.Close() }, nil
	}
}

// readFrames calls fn with every complete frame of n samples from r.
// It returns nil at the end of the input.
func readFrames(r io.Reader, n int, fn func([]int16)) error {
	buf := make([]byte, 2*n)
	samples := make([]int16, n)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("read audio: %w", err)
		}
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
		}
		fn(samples)
	}
}

// delayFrames passes frames on to out after n frames of delay, keeping only
// the newest one in out, so a slow sender skips frames instead of lagging.
func delayFrames(in <-chan [3]byte, out chan [3]byte, n int) {
	defer close(out)
	var queue [][3]byte
	for c := range in {
		queue = append(queue, c)
		if len(queue) <= n {
			continue
		}
		c, queue = queue[0], queue[1:]
		select {
		case <-out: // drop the frame the sender hasn't picked up yet
		default:
		}
		out <- c
	}
}

// sendFrames sends each new color to all lamps until frames is closed.
func sendFrames(ctx context.Context, cmd *cli.Command, lamps []lamp, sessions []*session, frames <-chan [3]byte) error {
	failures := make([]int, len(sessions))
	var last [3]byte
	first := true
	for c := range frames {
		if c == last && !first {
			continue
		}
		last, first = c, false
		for i, err := range broadcast(ctx, sessions, colorPacket(c)) {
			if sessions[i] == nil || ctx.Err() != nil {
				continue
			}
			if err == nil {
				failures[i] = 0
				continue
			}
			failures[i]++
			if failures[i] >= maxFailures {
				fmt.Fprintf(cmd.Root().ErrWriter, "govee: giving up on %s: %v\n", lamps[i].name, err)
				sessions[i].close()
				sessions[i] = nil
			}
		}
		if !hasSession(sessions) {
			return errors.New("no lamps left")
		}
	}
	return nil
}

func hasSession(sessions []*session) bool {
	for _, s := range sessions {
		if s != nil {
			return true
		}
	}
	return false
}

// analyzer turns audio frames into colors.
type analyzer struct {
	rate      float64 // frames per second
	alpha     float64 // low-pass filter coefficient
	lp1, lp2  float64 // two cascaded one-pole low-pass filters isolate the bass
	bassAvg   float64 // recent average bass level, for beat detection
	peak      float64 // recent peak loudness, for automatic gain
	level     float64 // current brightness, 0 to 1
	hue       float64 // degrees
	sinceBeat int     // frames
	peakDecay float64 // per-frame factors derived from the half-lives
	fade      float64
}

func newAnalyzer(rate float64) *analyzer {
	perFrame := func(halfLife time.Duration) float64 {
		return math.Pow(0.5, 1/(halfLife.Seconds()*rate))
	}
	return &analyzer{
		rate:      rate,
		alpha:     1 - math.Exp(-2*math.Pi*bassCutoff/sampleRate),
		sinceBeat: math.MaxInt32,
		peakDecay: perFrame(peakHalfLife),
		fade:      perFrame(fadeHalfLife),
	}
}

func (a *analyzer) next(samples []int16) [3]byte {
	var sum, bassSum float64
	for _, s := range samples {
		x := float64(s) / 32768
		a.lp1 += a.alpha * (x - a.lp1)
		a.lp2 += a.alpha * (a.lp1 - a.lp2)
		sum += x * x
		bassSum += a.lp2 * a.lp2
	}
	n := float64(len(samples))
	rms := math.Sqrt(sum / n)
	bass := math.Sqrt(bassSum / n)

	// A beat is a sudden rise of the bass above its recent (~1 s) average.
	a.sinceBeat++
	beat := bass > silence &&
		bass > beatRatio*a.bassAvg &&
		float64(a.sinceBeat) >= minBeatGap.Seconds()*a.rate
	a.bassAvg += (bass - a.bassAvg) * min(1/a.rate, 1)
	if beat {
		a.sinceBeat = 0
		a.hue = math.Mod(a.hue+goldenAngle, 360)
	}

	// Brightness follows loudness relative to the recent peak:
	// it jumps up immediately and fades out.
	a.peak = max(rms, a.peak*a.peakDecay, silence)
	loudness := 0.0
	if rms >= silence {
		loudness = rms / a.peak
	}
	a.level = max(loudness, a.level*a.fade)
	if beat {
		a.level = 1
	}
	return hsv(a.hue, minBrightness+(1-minBrightness)*math.Pow(a.level, gamma))
}

// hsv converts a fully saturated color with hue h (degrees)
// and brightness v (0 to 1) to RGB.
func hsv(h, v float64) [3]byte {
	x := v * (1 - math.Abs(math.Mod(h/60, 2)-1))
	var r, g, b float64
	switch {
	case h < 60:
		r, g = v, x
	case h < 120:
		r, g = x, v
	case h < 180:
		g, b = v, x
	case h < 240:
		g, b = x, v
	case h < 300:
		r, b = x, v
	default:
		r, b = v, x
	}
	to8 := func(f float64) byte { return byte(math.Round(min(max(f, 0), 1) * 255)) }
	return [3]byte{to8(r), to8(g), to8(b)}
}
