package tfo

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/database64128/tfo-go/bsd"
	"golang.org/x/sys/unix"
)

func SetTFOListener(fd uintptr) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_FASTOPEN, 1)
}

func (lc *ListenConfig) listenTFO(ctx context.Context, network, address string) (net.Listener, error) {
	ln, err := lc.ListenConfig.Listen(ctx, network, address)
	if err != nil {
		return nil, err
	}

	// darwin requires setting TCP_FASTOPEN after bind() and listen() calls.
	var innerErr error
	rawConn, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		ln.Close()
		return nil, err
	}
	err = rawConn.Control(func(fd uintptr) {
		innerErr = SetTFOListener(fd)
	})
	if err != nil {
		ln.Close()
		return nil, err
	}
	return ln, innerErr
}

func SetTFODialer(fd uintptr) error {
	return nil
}

func setKeepAlivePeriod(fd int, d time.Duration) error {
	// The kernel expects seconds so round to next highest second.
	secs := int(roundDurationUp(d, time.Second))
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, secs); err != nil {
		return err
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_KEEPALIVE, secs)
}

func socket(domain int) (fd int, err error) {
	fd, err = unix.Socket(domain, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	if err != nil {
		return
	}
	unix.CloseOnExec(fd)
	err = unix.SetNonblock(fd, true)
	if err != nil {
		unix.Close(fd)
		fd = 0
	}
	return
}

func (c *tfoConn) connect(b []byte) (n int, err error) {
	rawConn, err := c.f.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("failed to get syscall.RawConn: %w", err)
	}

	var done bool
	perr := rawConn.Write(func(fd uintptr) bool {
		if done {
			return true
		}

		bytesSent, err := bsd.Connectx(c.fd, 0, nil, c.rsockaddr, b)
		n = int(bytesSent)
		done = true
		if err == unix.EINPROGRESS {
			err = nil
			return false
		}
		return true
	})

	if err != nil {
		return 0, wrapSyscallError("connectx", err)
	}

	if perr != nil {
		return 0, perr
	}

	err = c.getSocketError("connectx")
	if err != nil {
		return
	}

	err = c.getLocalAddr()
	return
}
