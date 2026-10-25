//go:build !linux && !darwin

package main

import (
	"context"
	"errors"
	"io"
)

func recordSystem(context.Context) (io.Reader, func(), error) {
	return nil, nil, errors.New("recording what this machine is playing is implemented for Linux (parec) and macOS (Core Audio); pass --input with raw 48 kHz mono s16le audio")
}
