package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"
)

// bench measures how fast lamps accept color changes over one open connection.
// For each rate it alternates red and blue, then shows green as a separator,
// so that a person watching can tell the phases apart.
func bench(ctx context.Context, cmd *cli.Command) error {
	rates, err := parseRates(cmd.String("rates"))
	if err != nil {
		return err
	}
	lamps, err := resolveLamps(cmd.StringArgs("lamp"))
	if err != nil {
		return err
	}
	phase := cmd.Duration("phase")
	out := cmd.Root().Writer

	connectCtx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()
	sessions, results, err := connectAll(connectCtx, lamps, logger(cmd))
	if err != nil {
		return err
	}
	defer closeAll(sessions)
	var errs []error
	for i, l := range lamps {
		if results[i] != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.name, results[i]))
		}
	}
	if len(errs) == len(lamps) {
		return errors.Join(errs...)
	}
	for _, err := range errs {
		fmt.Fprintln(cmd.Root().ErrWriter, "govee: skipping", err)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tACHIEVED\tSENT\tERRORS\tACK MIN\tACK AVG\tACK MAX")
	for _, hz := range rates {
		r := benchPhase(ctx, sessions, hz, phase)
		fmt.Fprintf(w, "%g/s\t%.1f/s\t%d\t%d\t%v\t%v\t%v\n",
			hz, r.achieved, r.sent, r.errors, r.min.Round(time.Millisecond), r.avg().Round(time.Millisecond), r.max.Round(time.Millisecond))
		broadcast(ctx, sessions, colorPacket([3]byte{0, 255, 0}))
		time.Sleep(2 * time.Second)
	}
	return w.Flush()
}

type phaseResult struct {
	sent, errors  int
	achieved      float64 // updates per second
	min, max, sum time.Duration
}

func (r phaseResult) avg() time.Duration {
	if r.sent == 0 {
		return 0
	}
	return r.sum / time.Duration(r.sent)
}

// benchPhase alternates red and blue at hz updates per second for d.
// Each update waits for all lamps to acknowledge it; when that takes longer
// than the period, ticks are dropped and the achieved rate falls below hz.
func benchPhase(ctx context.Context, sessions []*session, hz float64, d time.Duration) phaseResult {
	colors := [][3]byte{{255, 0, 0}, {0, 0, 255}}
	ticker := time.NewTicker(time.Duration(float64(time.Second) / hz))
	defer ticker.Stop()
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	var r phaseResult
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			r.achieved = float64(r.sent) / time.Since(start).Seconds()
			return r
		case <-ticker.C:
		}
		t0 := time.Now()
		errs := broadcast(context.WithoutCancel(ctx), sessions, colorPacket(colors[r.sent%2]))
		took := time.Since(t0)
		for _, err := range errs {
			if err != nil {
				r.errors++
			}
		}
		if r.sent == 0 || took < r.min {
			r.min = took
		}
		r.max = max(r.max, took)
		r.sum += took
		r.sent++
	}
}

// parseRates parses a comma-separated list of positive rates, e.g. "1,2,5".
func parseRates(s string) ([]float64, error) {
	var rates []float64
	for f := range strings.SplitSeq(s, ",") {
		hz, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil || hz <= 0 {
			return nil, fmt.Errorf("invalid rate %q in %q (want positive numbers, e.g. 1,2,5)", f, s)
		}
		rates = append(rates, hz)
	}
	return rates, nil
}
