//go:build darwin

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework CoreAudio -framework Foundation
#include <stdlib.h>
int tap_start(int writeFD, char **err);
void tap_stop(void);
*/
import "C"

// recordSystem taps the mix of everything this Mac is playing
// and returns it as 48 kHz mono s16le.
func recordSystem(ctx context.Context) (io.Reader, func(), error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	// The tap owns the write end. Dup it so closing w does not close the tap's fd.
	fd, err := syscall.Dup(int(w.Fd()))
	w.Close()
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	var cerr *C.char
	if C.tap_start(C.int(fd), &cerr) != 0 {
		r.Close()
		msg := "record system audio"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.free(unsafe.Pointer(cerr))
		}
		return nil, nil, errors.New(msg)
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			C.tap_stop()
			r.Close()
		})
	}
	// Ctrl-C cancels ctx. Closing the tap's write end unblocks the reader.
	go func() {
		<-ctx.Done()
		stop()
	}()
	return r, stop, nil
}
