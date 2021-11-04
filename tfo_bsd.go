//go:build darwin || freebsd
// +build darwin freebsd

package tfo

import (
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type tfoConn struct {
	mu        sync.Mutex
	fd        int
	f         *os.File
	connected bool
	network   string
	laddr     *net.TCPAddr
	raddr     *net.TCPAddr
	lsockaddr unix.Sockaddr
	rsockaddr unix.Sockaddr
}

func setIPv6Only(fd int, family int, ipv6only int) error {
	if family == unix.AF_INET6 {
		// Allow both IP versions even if the OS default
		// is otherwise. Note that some operating systems
		// never admit this option.
		unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, ipv6only)
	}
	return nil
}

func setNoDelay(fd int, noDelay int) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, noDelay)
}

func setKeepAlive(fd int, keepalive int) error {
	return unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_KEEPALIVE, keepalive)
}

func setLinger(fd int, sec int) error {
	var l unix.Linger
	if sec >= 0 {
		l.Onoff = 1
		l.Linger = int32(sec)
	} else {
		l.Onoff = 0
		l.Linger = 0
	}
	return unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &l)
}

func dialTFO(network string, laddr, raddr *net.TCPAddr) (TFOConn, error) {
	var domain int
	var lsockaddr, rsockaddr unix.Sockaddr

	raddrIs4 := raddr.IP.To4() != nil
	if raddrIs4 {
		domain = unix.AF_INET
		rsockaddr = &unix.SockaddrInet4{
			Port: raddr.Port,
			Addr: *(*[4]byte)(raddr.IP),
		}
	} else {
		domain = unix.AF_INET6
		rsockaddr = &unix.SockaddrInet6{
			Port: raddr.Port,
			Addr: *(*[16]byte)(raddr.IP),
		}
	}

	if laddr != nil {
		laddrIs4 := laddr.IP.To4() != nil
		if laddrIs4 != raddrIs4 {
			return nil, ErrMismatchedAddressFamily
		}
		if laddrIs4 {
			lsockaddr = &unix.SockaddrInet4{
				Port: laddr.Port,
				Addr: *(*[4]byte)(laddr.IP),
			}
		} else {
			lsockaddr = &unix.SockaddrInet6{
				Port: laddr.Port,
				Addr: *(*[16]byte)(laddr.IP),
			}
		}
	} else if raddrIs4 {
		lsockaddr = &unix.SockaddrInet4{}
	} else {
		lsockaddr = &unix.SockaddrInet6{}
	}

	fd, err := socket(domain)
	if err != nil {
		return nil, wrapSyscallError("socket", err)
	}

	var v6only int
	if network == "tcp6" {
		v6only = 1
	}

	if err := setIPv6Only(fd, domain, v6only); err != nil {
		return nil, wrapSyscallError("setsockopt", err)
	}

	if err := setNoDelay(fd, 1); err != nil {
		return nil, wrapSyscallError("setsockopt", err)
	}

	if err := SetTFODialer(uintptr(fd)); err != nil {
		return nil, wrapSyscallError("setsockopt", err)
	}

	if laddr != nil {
		if err := unix.Bind(fd, lsockaddr); err != nil {
			return nil, wrapSyscallError("bind", err)
		}
	}

	f := os.NewFile(uintptr(fd), "")

	return &tfoConn{
		fd:        fd,
		f:         f,
		network:   network,
		laddr:     laddr,
		raddr:     raddr,
		lsockaddr: lsockaddr,
		rsockaddr: rsockaddr,
	}, err
}

func (c *tfoConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if !c.connected {
		_, err := c.connect(nil)
		if err != nil {
			c.mu.Unlock()
			return 0, err
		}
		c.connected = true
	}
	c.mu.Unlock()
	return c.f.Read(b)
}

// ReadFrom utilizes the underlying file's ReadFrom method to minimize copies and allocations.
// This method does not send data in SYN, because application protocols usually write headers
// before calling ReadFrom/WriteTo.
func (c *tfoConn) ReadFrom(r io.Reader) (int64, error) {
	c.mu.Lock()
	if !c.connected {
		_, err := c.connect(nil)
		if err != nil {
			c.mu.Unlock()
			return 0, err
		}
		c.connected = true
	}
	c.mu.Unlock()
	return c.f.ReadFrom(r)
}

func (c *tfoConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return c.f.Write(b)
	}

	n, err := c.connect(b)
	if err == nil {
		c.connected = true
	}
	c.mu.Unlock()
	return n, err
}

func (c *tfoConn) Close() error {
	if err := c.f.Close(); err != nil {
		return &net.OpError{Op: "close", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: err}
	}
	return nil
}

func (c *tfoConn) CloseRead() error {
	if err := unix.Shutdown(c.fd, unix.SHUT_RD); err != nil {
		return &net.OpError{Op: "close", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: wrapSyscallError("shutdown", err)}
	}
	return nil
}

func (c *tfoConn) CloseWrite() error {
	if err := unix.Shutdown(c.fd, unix.SHUT_WR); err != nil {
		return &net.OpError{Op: "close", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: wrapSyscallError("shutdown", err)}
	}
	return nil
}

func (c *tfoConn) LocalAddr() net.Addr {
	return c.laddr
}

func (c *tfoConn) RemoteAddr() net.Addr {
	return c.raddr
}

func (c *tfoConn) SetDeadline(t time.Time) error {
	if err := c.f.SetDeadline(t); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: nil, Addr: c.laddr, Err: err}
	}
	return nil
}

func (c *tfoConn) SetReadDeadline(t time.Time) error {
	if err := c.f.SetReadDeadline(t); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: nil, Addr: c.laddr, Err: err}
	}
	return nil
}

func (c *tfoConn) SetWriteDeadline(t time.Time) error {
	if err := c.f.SetWriteDeadline(t); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: nil, Addr: c.laddr, Err: err}
	}
	return nil
}

func (c *tfoConn) SetNoDelay(noDelay bool) error {
	var value int
	if noDelay {
		value = 1
	}
	if err := setNoDelay(c.fd, value); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: wrapSyscallError("setsockopt", err)}
	}
	return nil
}

func (c *tfoConn) SetKeepAlive(keepalive bool) error {
	var value int
	if keepalive {
		value = 1
	}
	if err := setKeepAlive(c.fd, value); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: wrapSyscallError("setsockopt", err)}
	}
	return nil
}

func (c *tfoConn) SetKeepAlivePeriod(d time.Duration) error {
	if err := setKeepAlivePeriod(c.fd, d); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: wrapSyscallError("setsockopt", err)}
	}
	return nil
}

func (c *tfoConn) SetLinger(sec int) error {
	if err := setLinger(c.fd, sec); err != nil {
		return &net.OpError{Op: "set", Net: c.network, Source: c.laddr, Addr: c.raddr, Err: wrapSyscallError("setsockopt", err)}
	}
	return nil
}
