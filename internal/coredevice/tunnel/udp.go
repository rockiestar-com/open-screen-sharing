package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

const (
	protocolUDP     = 17
	udpHeaderLength = 8
)

// sing-tun drops datagrams for a UDP socket once 212992 bytes are queued;
// on iOS, SetReadBuffer can raise that to at most 256 KiB
// (sing-tun v0.9.7-0.20261002083955-3f8acd9da65b, stack_go_udp_socket.go:26,
// 91; stack_go_udp_socket.go:172-174,
// 445; stack_go_profile_ios.go:7).
const udpQueueSize = 8 << 20

type UDPConn struct {
	tunnel  *Tunnel
	conn    *tun.GoUDPConn
	port    uint16
	options N.ReadWaitOptions
	queue   *datagramQueue
}

func (t *Tunnel) ListenUDP(port uint16, options N.ReadWaitOptions) (*UDPConn, error) {
	conn, err := t.stack.ListenUDP(netip.AddrPortFrom(t.parameters.ClientAddress, port))
	if err != nil {
		return nil, err
	}
	conn.InitializeReadWaiter(options)
	c := &UDPConn{
		tunnel:  t,
		conn:    conn,
		port:    uint16(conn.LocalAddr().(*net.UDPAddr).Port),
		options: options,
		queue:   newDatagramQueue(),
	}
	t.udpAccess.Lock()
	select {
	case <-t.done:
		t.udpAccess.Unlock()
		conn.Close()
		return nil, E.Cause(t.err, "tunnel closed")
	default:
	}
	t.udpConns[c.port] = c
	t.udpAccess.Unlock()
	go c.receiveFromStack()
	return c, nil
}

func (c *UDPConn) receiveFromStack() {
	for {
		buffer, source, err := c.conn.WaitReadPacket()
		if err != nil {
			return
		}
		c.queue.push(Datagram{Buffer: buffer, Source: source.AddrPort(), Arrived: time.Now()})
	}
}

func (c *UDPConn) Port() uint16 {
	return c.port
}

type Datagram struct {
	Buffer  *buf.Buffer
	Source  netip.AddrPort
	Arrived time.Time
}

func (c *UDPConn) ReadBatch(ctx context.Context, datagrams []Datagram) (int, error) {
	for {
		n, err := c.queue.pop(datagrams)
		if !errors.Is(err, errQueueEmpty) {
			return n, err
		}
		select {
		case <-c.queue.ready:
		case <-c.queue.closed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (c *UDPConn) WriteTo(buffer []byte, address netip.AddrPort) (int, error) {
	return c.conn.WriteToUDPAddrPort(buffer, address)
}

func (c *UDPConn) CloseRead() {
	c.queue.close(net.ErrClosed)
}

func (c *UDPConn) Close() error {
	t := c.tunnel
	t.udpAccess.Lock()
	delete(t.udpConns, c.port)
	t.udpAccess.Unlock()
	c.queue.close(net.ErrClosed)
	err := c.conn.Close()
	if dropped := c.queue.droppedCount(); dropped > 0 {
		t.logger.Debug("dropped datagrams that found the queue full",
			slog.Int("port", int(c.port)), slog.Uint64("count", dropped))
	}
	return err
}

func (t *Tunnel) deliverDirect(packet []byte, arrived time.Time) bool {
	if len(packet) < ipv6HeaderLength+udpHeaderLength || packet[6] != protocolUDP ||
		!bytes.Equal(packet[24:40], t.clientAddress[:]) {
		return false
	}
	udp := packet[ipv6HeaderLength:]
	t.udpAccess.RLock()
	c := t.udpConns[binary.BigEndian.Uint16(udp[2:])]
	t.udpAccess.RUnlock()
	if c == nil {
		return false
	}
	length := int(binary.BigEndian.Uint16(udp[4:]))
	if length < udpHeaderLength || length > len(udp) {
		return false
	}
	payload := udp[udpHeaderLength:length]
	buffer := c.options.NewBufferSize(len(payload))
	copy(buffer.Extend(len(payload)), payload)
	c.options.PostReturn(buffer)
	source := netip.AddrPortFrom(netip.AddrFrom16([16]byte(packet[8:24])).Unmap(), binary.BigEndian.Uint16(udp[0:]))
	c.queue.push(Datagram{Buffer: buffer, Source: source, Arrived: arrived})
	return true
}

func (t *Tunnel) closeUDP(err error) {
	t.udpAccess.Lock()
	defer t.udpAccess.Unlock()
	for _, c := range t.udpConns {
		c.queue.close(E.Cause(err, "tunnel closed"))
	}
}

var errQueueEmpty = E.New("queue empty")

type datagramQueue struct {
	access    sync.Mutex
	datagrams []Datagram
	head      int
	size      int
	dropped   uint64
	err       error
	ready     chan struct{}
	closed    chan struct{}
}

func newDatagramQueue() *datagramQueue {
	return &datagramQueue{
		ready:  make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

func (q *datagramQueue) push(datagram Datagram) {
	size := datagram.Buffer.RawCap()
	q.access.Lock()
	if q.err != nil || q.size+size > udpQueueSize {
		if q.err == nil {
			q.dropped++
		}
		q.access.Unlock()
		datagram.Buffer.Release()
		return
	}
	wasEmpty := q.head == len(q.datagrams)
	q.datagrams = append(q.datagrams, datagram)
	q.size += size
	q.access.Unlock()
	if wasEmpty {
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
}

func (q *datagramQueue) pop(datagrams []Datagram) (int, error) {
	q.access.Lock()
	defer q.access.Unlock()
	if q.err != nil {
		return 0, q.err
	}
	n := copy(datagrams, q.datagrams[q.head:])
	if n == 0 {
		return 0, errQueueEmpty
	}
	for _, datagram := range datagrams[:n] {
		q.size -= datagram.Buffer.RawCap()
	}
	q.head += n
	if q.head >= len(q.datagrams)/2 {
		remaining := copy(q.datagrams, q.datagrams[q.head:])
		clear(q.datagrams[remaining:])
		q.datagrams = q.datagrams[:remaining]
		q.head = 0
	}
	return n, nil
}

func (q *datagramQueue) close(err error) {
	q.access.Lock()
	defer q.access.Unlock()
	if q.err != nil {
		return
	}
	q.err = err
	for _, datagram := range q.datagrams[q.head:] {
		datagram.Buffer.Release()
	}
	q.datagrams, q.head, q.size = nil, 0, 0
	close(q.closed)
}

func (q *datagramQueue) droppedCount() uint64 {
	q.access.Lock()
	defer q.access.Unlock()
	return q.dropped
}
