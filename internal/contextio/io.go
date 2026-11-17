// Package contextio owns cancellable CLI streams and bounded reads.
package contextio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// Stream is a private descriptor for an invocation's inherited stream. Pipes
// and sockets are made pollable before NewFile: inherited blocking descriptors
// otherwise need not support deadlines, and closing them cannot stop a read.
// Callers must own the stream for the duration; dup shares its status flags.
type Stream struct {
	file      *os.File
	original  syscall.RawConn
	ctx       context.Context
	stop      func() bool
	done      chan struct{}
	flags     int
	closeOnce sync.Once
	closeErr  error
}

// Own duplicates f, makes peer streams pollable, and closes the duplicate on
// cancellation. It starts no worker that can remain blocked in an I/O syscall.
func Own(ctx context.Context, f *os.File) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd, flags := -1, 0
	var operation error
	err = raw.Control(func(original uintptr) {
		value, _, errno := syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_GETFL, 0)
		if errno != 0 {
			operation = errno
			return
		}
		flags = int(value)
		// Fd() would permanently switch a Go-pollable original to blocking.
		// Control preserves both its flags and its Go poller bookkeeping.
		syscall.ForkLock.RLock()
		fd, operation = syscall.Dup(int(original))
		if operation == nil {
			syscall.CloseOnExec(fd)
		}
		syscall.ForkLock.RUnlock()
	})
	if err != nil {
		return nil, err
	}
	if operation != nil {
		return nil, operation
	}
	peer := st.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0
	if peer {
		if err := syscall.SetNonblock(fd, true); err != nil {
			syscall.Close(fd)
			return nil, err
		}
	}
	s := &Stream{file: os.NewFile(uintptr(fd), f.Name()), original: raw, ctx: ctx, flags: flags, done: make(chan struct{})}
	if peer {
		if err := s.file.SetDeadline(time.Time{}); err != nil {
			_ = s.file.Close()
			_ = s.restore()
			return nil, fmt.Errorf("stream cannot be canceled: %w", err)
		}
	}
	s.stop = context.AfterFunc(ctx, func() { _ = s.file.Close(); close(s.done) })
	return s, nil
}

func (s *Stream) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.file.Read(p)
	if s.ctx.Err() != nil {
		return 0, s.ctx.Err()
	}
	return n, err
}

func (s *Stream) Write(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.file.Write(p)
	if s.ctx.Err() != nil {
		return n, s.ctx.Err()
	}
	return n, err
}

// Close waits for the cancellation callback, closes the owned descriptor and
// restores status flags shared with the inherited descriptor.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		if !s.stop() {
			<-s.done
		}
		err := s.file.Close()
		if errors.Is(err, os.ErrClosed) {
			err = nil
		}
		s.closeErr = errors.Join(err, s.restore())
	})
	return s.closeErr
}

func (s *Stream) restore() error {
	var operation error
	err := s.original.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFL, uintptr(s.flags))
		if errno != 0 {
			operation = errno
		}
	})
	return errors.Join(err, operation)
}

// ReadAtMost reads r to its end, failing when it holds more than limit bytes.
// It bounds allocation and work, not time: a reader that may block is an
// owned stream (Own), which cancellation closes.
func ReadAtMost(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("input exceeds %d bytes", limit)
	}
	return b, err
}

// ReadFile reads a file of at most limit bytes, if ctx is still live.
func ReadFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAtMost(f, limit)
}
