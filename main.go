// Command govee controls Govee H6006 smart bulbs over Bluetooth LE.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"
)

func main() {
	cmd := &cli.Command{
		Name:  "govee",
		Usage: "control Govee H6006 bulbs over Bluetooth LE",
		Description: "LAMP is a lamp name (table, drawer), \"all\", or a BLE MAC address.\n" +
			"Without LAMP arguments, a command applies to all lamps.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "print the plaintext packets instead of sending them",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "log protocol traffic to stderr",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "give up after this long",
				Value: 30 * time.Second,
			},
		},
		Commands: []*cli.Command{
			{
				Name:      "on",
				Usage:     "turn lamps on",
				Arguments: []cli.Argument{lampsArg()},
				Action:    send(func(*cli.Command) ([]byte, error) { return powerPacket(true), nil }),
			},
			{
				Name:      "off",
				Usage:     "turn lamps off",
				Arguments: []cli.Argument{lampsArg()},
				Action:    send(func(*cli.Command) ([]byte, error) { return powerPacket(false), nil }),
			},
			{
				Name:  "color",
				Usage: "set the color of lamps",
				Arguments: []cli.Argument{
					&cli.StringArg{Name: "color", UsageText: "COLOR", Required: true},
					lampsArg(),
				},
				Action: send(func(cmd *cli.Command) ([]byte, error) {
					rgb, err := parseColor(cmd.StringArg("color"))
					if err != nil {
						return nil, err
					}
					return colorPacket(rgb), nil
				}),
			},
			{
				Name:        "raw",
				Usage:       "send a raw command, e.g. 330101 (power on)",
				Description: "HEX is the packet without its checksum, which is added automatically.",
				Arguments: []cli.Argument{
					&cli.StringArg{Name: "hex", UsageText: "HEX", Required: true},
					lampsArg(),
				},
				Action: send(func(cmd *cli.Command) ([]byte, error) {
					payload, err := parseRaw(cmd.StringArg("hex"))
					if err != nil {
						return nil, err
					}
					return packet(payload...), nil
				}),
			},
			{
				Name:  "scan",
				Usage: "list nearby Govee devices",
				Flags: []cli.Flag{
					&cli.DurationFlag{
						Name:  "duration",
						Usage: "how long to scan",
						Value: 5 * time.Second,
					},
				},
				Action: scan,
			},
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "govee:", err)
		os.Exit(1)
	}
}

func lampsArg() *cli.StringArgs {
	return &cli.StringArgs{Name: "lamp", UsageText: "[LAMP...]", Min: 0, Max: -1}
}

// send returns an action that sends the packet built by build to the lamps.
func send(build func(*cli.Command) ([]byte, error)) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		payload, err := build(cmd)
		if err != nil {
			return err
		}
		lamps, err := resolveLamps(cmd.StringArgs("lamp"))
		if err != nil {
			return err
		}
		out := cmd.Root().Writer

		if cmd.Bool("dry-run") {
			for _, l := range lamps {
				fmt.Fprintf(out, "%s %s % x\n", l.name, l.addr, payload)
			}
			return nil
		}

		ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
		defer cancel()
		results, err := sendAll(ctx, lamps, payload, logger(cmd))
		if err != nil {
			return err
		}
		var errs []error
		for i, l := range lamps {
			if results[i] != nil {
				errs = append(errs, fmt.Errorf("%s: %w", l.name, results[i]))
				continue
			}
			fmt.Fprintf(out, "%s: ok\n", l.name)
		}
		return errors.Join(errs...)
	}
}

func scan(ctx context.Context, cmd *cli.Command) error {
	devices, err := discover(ctx, cmd.Duration("duration"))
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.Root().Writer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ADDRESS\tRSSI\tNAME\tLAMP")
	for _, d := range devices {
		name := lampName(d.addr)
		if name == d.addr {
			name = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", d.addr, d.rssi, d.name, name)
	}
	return w.Flush()
}

func logger(cmd *cli.Command) logFunc {
	if !cmd.Bool("verbose") {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(cmd.Root().ErrWriter, format+"\n", args...)
	}
}
