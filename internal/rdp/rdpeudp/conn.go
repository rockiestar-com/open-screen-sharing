package rdpeudp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"math"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

const (
	flagSYN   = 0x0001
	flagACK   = 0x0004
	flagSYNEX = 0x1000

	synExVersionInfoValid = 0x0001
	version1              = 0x0001
	version2              = 0x0002
	version3              = 0x0101

	headerLength  = 8
	synDataLength = 8

	mtu    = 1232
	window = 256

	initialRTO       = time.Second
	minimumRTO       = 300 * time.Millisecond
	minimumRTO1      = 500 * time.Millisecond
	maximumRTO       = 16 * time.Second
	handshakeRetries = 5
	keepAlive        = 4 * time.Second
	idleTimeout      = 65 * time.Second
	ackDelayMinimum  = 10 * time.Millisecond
	ackDelayMaximum  = 100 * time.Millisecond
	reorderThreshold = 3
	initialWindow    = 10 * mtu
	minimumWindow    = 2 * mtu
)

type outgoing struct {
	seq       uint64
	order     uint64
	data      []byte
	sent      time.Time
	transmits int
}

type arrival struct {
	seq uint64
	at  time.Time
}

type Conn struct {
	socket      *net.UDPConn
	logger      *slog.Logger
	established chan struct{}
	done        chan struct{}
	timer       *time.Timer

	access   sync.Mutex
	readable sync.Cond
	err      error

	start            time.Time
	localISN         uint32
	remoteISN        uint32
	version          uint16
	connected        bool
	handshake        []byte
	handshakeSent    time.Time
	handshakeRetries int
	maxPayload       int
	floorRTO         time.Duration

	srtt         time.Duration
	rttvar       time.Duration
	rto          time.Duration
	retransmitAt time.Time
	keepAliveAt  time.Time
	idleAt       time.Time
	ackAt        time.Time
	ackSince     time.Time

	nextSeq           uint64
	nextOrder         uint64
	nextCoded         uint32
	queue             [][]byte
	inflight          []*outgoing
	lost              []*outgoing
	inflightBytes     int
	congestionWindow  int
	slowStart         int
	avoidance         int
	recovery          uint64
	highestAckedOrder uint64
	congestionReduced bool
	remoteWindow      int
	sinceAckOfAcks    int
	ackOfAcksSent     uint64
	peerBase          uint64

	receiveBase    uint64
	receiveState   []bool
	receiveHighest uint64
	highestArrival time.Time
	arrivals       []arrival
	nextDelivery   uint64
	reorder        map[uint64][]byte
	readQueue      [][]byte
	unacknowledged int
	ackEvery       int
	ackTimeout     time.Duration
	ackNow         bool

	sentPackets      uint64
	retransmits      uint64
	receivedPackets  uint64
	sequencedPackets uint64
	missedPackets    uint64
}

func Dial(ctx context.Context, address string, cookie []byte, logger *slog.Logger) (*Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	var isn [4]byte
	_, _ = rand.Read(isn[:])
	now := time.Now()
	c := &Conn{
		socket:           conn.(*net.UDPConn),
		logger:           logger,
		established:      make(chan struct{}),
		done:             make(chan struct{}),
		start:            now,
		localISN:         binary.BigEndian.Uint32(isn[:]),
		floorRTO:         minimumRTO,
		rto:              initialRTO,
		congestionWindow: initialWindow,
		slowStart:        math.MaxInt,
		remoteWindow:     window,
		reorder:          make(map[uint64][]byte),
	}
	c.readable.L = &c.access
	c.handshake = binary.BigEndian.AppendUint32(nil, math.MaxUint32)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, window)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, flagSYN|flagSYNEX)
	c.handshake = binary.BigEndian.AppendUint32(c.handshake, c.localISN)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, mtu)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, mtu)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, synExVersionInfoValid)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, version3)
	hash := sha256.Sum256(cookie)
	// rdpbase.dll of Windows 11 26200 compares the cookie hash with the SHA-256 digest taken as eight
	// little-endian 32-bit words, so the bytes of each word go out reversed, and silently drops a SYN
	// whose hash does not match instead of falling back to version 2.
	for word := range slices.Chunk(hash[:], 4) {
		slices.Reverse(word)
	}
	c.handshake = append(c.handshake, hash[:]...)
	c.handshake = append(c.handshake, make([]byte, mtu-len(c.handshake))...)
	c.access.Lock()
	c.timer = time.AfterFunc(c.rto, c.expire)
	c.handshakeSent = now
	c.retransmitAt = now.Add(c.rto)
	c.idleAt = now.Add(idleTimeout)
	c.write(c.handshake)
	c.access.Unlock()
	go c.receive()
	select {
	case <-c.established:
		return c, nil
	case <-c.done:
		return nil, c.err
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

func (c *Conn) Read(b []byte) (int, error) {
	c.access.Lock()
	defer c.access.Unlock()
	for len(c.readQueue) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		c.readable.Wait()
	}
	n := copy(b, c.readQueue[0])
	if n == len(c.readQueue[0]) {
		c.readQueue[0] = nil
		c.readQueue = c.readQueue[1:]
	} else {
		c.readQueue[0] = c.readQueue[0][n:]
	}
	return n, nil
}

func (c *Conn) Write(b []byte) (int, error) {
	c.access.Lock()
	defer c.access.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	for chunk := range slices.Chunk(b, c.maxPayload) {
		c.queue = append(c.queue, slices.Clone(chunk))
	}
	now := time.Now()
	c.flush(now)
	c.arm(now)
	return len(b), nil
}

func (c *Conn) Close() error {
	c.access.Lock()
	defer c.access.Unlock()
	c.fail(net.ErrClosed)
	return nil
}

func (c *Conn) LocalAddr() net.Addr {
	return c.socket.LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.socket.RemoteAddr()
}

func (c *Conn) SetDeadline(time.Time) error {
	return os.ErrNoDeadline
}

func (c *Conn) SetReadDeadline(time.Time) error {
	return os.ErrNoDeadline
}

func (c *Conn) SetWriteDeadline(time.Time) error {
	return os.ErrNoDeadline
}

func (c *Conn) RoundTrip() time.Duration {
	c.access.Lock()
	defer c.access.Unlock()
	return c.srtt
}

func (c *Conn) Packets() (expected, missed uint64) {
	c.access.Lock()
	defer c.access.Unlock()
	return c.sequencedPackets, c.missedPackets
}

func (c *Conn) fail(err error) {
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
	c.timer.Stop()
	_ = c.socket.Close()
	c.readable.Broadcast()
	c.logger.Debug("close the UDP transport", "error", err, "version", c.version, "sent", c.sentPackets, "retransmitted", c.retransmits, "received", c.receivedPackets, "rtt", c.srtt)
}

func (c *Conn) write(datagram []byte) {
	_, err := c.socket.Write(datagram)
	if err != nil {
		c.logger.Debug("send a UDP datagram", "error", err)
	}
}

func (c *Conn) receive() {
	buffer := make([]byte, 2*mtu)
	for {
		n, err := c.socket.Read(buffer)
		now := time.Now()
		c.access.Lock()
		if err != nil {
			c.fail(err)
			c.access.Unlock()
			return
		}
		if c.err == nil && n >= headerLength {
			c.idleAt = now.Add(idleTimeout)
			switch {
			case !c.connected:
				c.receiveSynAck(buffer[:n], now)
			case c.synAckRepeated(buffer[:n]):
				if c.version != version3 {
					c.write(c.handshake)
				}
			case c.version == version3:
				c.receivePacket(buffer[:n], now)
			case binary.BigEndian.Uint16(buffer[6:])&flagSYN == 0:
				c.receiveDatagram(buffer[:n], now)
			}
			c.flush(now)
			c.arm(now)
		}
		c.access.Unlock()
	}
}

func (c *Conn) receiveSynAck(datagram []byte, now time.Time) {
	receiveWindow := binary.BigEndian.Uint16(datagram[4:])
	flags := binary.BigEndian.Uint16(datagram[6:])
	if flags&(flagSYN|flagACK) != flagSYN|flagACK || len(datagram) < headerLength+synDataLength || binary.BigEndian.Uint32(datagram) != c.localISN {
		return
	}
	synData := datagram[headerLength:]
	remoteISN := binary.BigEndian.Uint32(synData)
	negotiated := min(int(binary.BigEndian.Uint16(synData[4:])), int(binary.BigEndian.Uint16(synData[6:])), mtu)
	extension := synData[synDataLength:]
	version := uint16(version1)
	if flags&flagSYNEX != 0 && len(extension) >= 4 && binary.BigEndian.Uint16(extension)&synExVersionInfoValid != 0 {
		version = binary.BigEndian.Uint16(extension[2:])
	}
	overhead := dataOverhead
	switch version {
	case version1:
		c.floorRTO = minimumRTO1
	case version2:
	case version3:
		overhead = dataOverhead2
	default:
		c.fail(E.New("rdpeudp: the server selected unsupported protocol version 0x", strconv.FormatUint(uint64(version), 16)))
		return
	}
	if negotiated <= overhead {
		c.fail(E.New("rdpeudp: the server MTU of ", negotiated, " bytes is too small"))
		return
	}
	if c.handshakeRetries == 0 {
		c.sample(now.Sub(c.handshakeSent))
	}
	c.version = version
	c.remoteISN = remoteISN
	c.maxPayload = negotiated - overhead
	c.connected = true
	c.retransmitAt = time.Time{}
	c.keepAliveAt = now.Add(keepAlive)
	if version == version3 {
		c.connectUDP2()
	} else {
		c.connectUDP1(receiveWindow)
	}
	close(c.established)
	c.logger.Debug("connect the UDP transport", "version", version, "rtt", c.srtt, "mtu", negotiated, "server_window", receiveWindow)
}

func (c *Conn) synAckRepeated(datagram []byte) bool {
	return len(datagram) >= headerLength+synDataLength &&
		binary.BigEndian.Uint32(datagram) == c.localISN &&
		binary.BigEndian.Uint16(datagram[6:])&(flagSYN|flagACK) == flagSYN|flagACK &&
		binary.BigEndian.Uint32(datagram[headerLength:]) == c.remoteISN
}

func (c *Conn) acknowledge(received func(p *outgoing) bool) *outgoing {
	var newly int
	var newest *outgoing
	inflight := c.inflight[:0]
	for _, p := range c.inflight {
		if !received(p) {
			inflight = append(inflight, p)
			continue
		}
		newly += len(p.data)
		c.highestAckedOrder = max(c.highestAckedOrder, p.order)
		if newest == nil || p.order > newest.order {
			newest = p
		}
	}
	clear(c.inflight[len(inflight):])
	c.inflight = inflight
	if newly > 0 {
		c.inflightBytes -= newly
		if c.congestionWindow < c.slowStart {
			c.congestionWindow += newly
		} else {
			c.avoidance += newly
			if c.avoidance >= c.congestionWindow {
				c.avoidance -= c.congestionWindow
				c.congestionWindow += mtu
			}
		}
		c.retransmitAt = time.Time{}
	}
	return newest
}

func (c *Conn) detectLoss(now time.Time) {
	threshold := c.rto * 5 / 4
	lastLost := -1
	for i, p := range c.inflight {
		if c.highestAckedOrder >= p.order+reorderThreshold || now.Sub(p.sent) > threshold {
			lastLost = i
		}
	}
	if lastLost >= 0 {
		c.declareLost(lastLost + 1)
	}
	c.updateRetransmit(now)
}

func (c *Conn) declareLost(count int) {
	lost := c.inflight[:count]
	c.inflight = slices.Clone(c.inflight[count:])
	for _, p := range lost {
		c.inflightBytes -= len(p.data)
	}
	c.lost = append(c.lost, lost...)
	if lost[0].order >= c.recovery {
		c.recovery = c.nextOrder
		c.reduce()
	}
}

func (c *Conn) reduce() {
	c.slowStart = max(c.congestionWindow/2, minimumWindow)
	c.congestionWindow = c.slowStart
	c.avoidance = 0
}

func (c *Conn) updateRetransmit(now time.Time) {
	if len(c.inflight) == 0 && len(c.lost) == 0 {
		c.retransmitAt = time.Time{}
	} else if c.retransmitAt.IsZero() {
		c.retransmitAt = now.Add(c.rto)
	}
}

func (c *Conn) windowOpen() bool {
	if c.version == version3 {
		return c.nextOrder < c.oldestPacket()+uint64(c.remoteWindow)
	}
	return c.nextSeq < c.oldestSource()+uint64(c.remoteWindow)
}

func (c *Conn) deliver(seq uint64, payload []byte) {
	if seq < c.nextDelivery {
		return
	}
	c.reorder[seq] = slices.Clone(payload)
	delivered := false
	for {
		data, found := c.reorder[c.nextDelivery]
		if !found {
			break
		}
		delete(c.reorder, c.nextDelivery)
		c.nextDelivery = c.following(c.nextDelivery)
		if len(data) > 0 {
			c.readQueue = append(c.readQueue, data)
			delivered = true
		}
	}
	if delivered {
		c.readable.Broadcast()
	}
}

func (c *Conn) following(seq uint64) uint64 {
	seq++
	if c.version == version3 && uint16(seq) == 0 {
		seq++
	}
	return seq
}

func (c *Conn) sample(rtt time.Duration) {
	if c.srtt == 0 {
		c.srtt = rtt
		c.rttvar = rtt / 2
	} else {
		c.rttvar = (3*c.rttvar + (c.srtt - rtt).Abs()) / 4
		c.srtt = (7*c.srtt + rtt) / 8
	}
	c.rto = min(max(c.srtt+4*c.rttvar, c.floorRTO), maximumRTO)
}

func (c *Conn) ackDelay() time.Duration {
	if c.ackTimeout > 0 {
		return c.ackTimeout
	}
	return min(max(c.srtt/2, ackDelayMinimum), ackDelayMaximum)
}

func (c *Conn) expire() {
	c.access.Lock()
	defer c.access.Unlock()
	if c.err != nil {
		return
	}
	now := time.Now()
	if !now.Before(c.idleAt) {
		c.fail(E.New("rdpeudp: no packets received for ", idleTimeout))
		return
	}
	if !c.retransmitAt.IsZero() && !now.Before(c.retransmitAt) {
		c.retransmitAt = time.Time{}
		c.rto = min(2*c.rto, maximumRTO)
		if !c.connected {
			if c.handshakeRetries >= handshakeRetries {
				c.fail(E.New("rdpeudp: the server did not answer the handshake"))
				return
			}
			c.handshakeRetries++
			c.write(c.handshake)
			c.retransmitAt = now.Add(c.rto)
		} else {
			if len(c.inflight) > 0 {
				c.declareLost(1)
			}
			c.updateRetransmit(now)
		}
	}
	c.flush(now)
	c.arm(now)
}

func (c *Conn) arm(now time.Time) {
	if c.err != nil {
		return
	}
	deadline := c.idleAt
	for _, at := range []time.Time{c.retransmitAt, c.keepAliveAt} {
		if !at.IsZero() && at.Before(deadline) {
			deadline = at
		}
	}
	if c.unacknowledged > 0 && c.ackAt.Before(deadline) {
		deadline = c.ackAt
	}
	c.timer.Reset(max(deadline.Sub(now), 0))
}

func (c *Conn) flush(now time.Time) {
	if c.err != nil || !c.connected {
		return
	}
	for c.inflightBytes < c.congestionWindow {
		var p *outgoing
		if len(c.lost) > 0 {
			p = c.lost[0]
			c.lost[0] = nil
			c.lost = c.lost[1:]
			c.retransmits++
			c.retransmitAt = now.Add(c.rto)
		} else if len(c.queue) > 0 && c.windowOpen() {
			p = &outgoing{seq: c.nextSeq, data: c.queue[0]}
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.nextSeq = c.following(c.nextSeq)
			if c.retransmitAt.IsZero() {
				c.retransmitAt = now.Add(c.rto)
			}
		} else {
			break
		}
		p.transmits++
		p.order = c.nextOrder
		p.sent = now
		c.nextOrder++
		c.inflight = append(c.inflight, p)
		c.inflightBytes += len(p.data)
		c.send(p, now)
	}
	if c.ackNow || c.unacknowledged >= c.ackEvery || c.unacknowledged > 0 && !now.Before(c.ackAt) || !now.Before(c.keepAliveAt) {
		c.send(nil, now)
	}
}

func (c *Conn) send(p *outgoing, now time.Time) {
	if c.version == version3 {
		c.sendPacket(p, now)
	} else {
		c.sendDatagram(p, now)
	}
	c.sentPackets++
	c.keepAliveAt = now.Add(keepAlive)
	c.unacknowledged = 0
	c.ackNow = false
	c.ackAt = time.Time{}
}

func reconstruct(wire uint64, bits uint, reference uint64) uint64 {
	space := uint64(1) << bits
	candidate := reference&^(space-1) | wire
	switch {
	case candidate > reference && candidate-reference > space/2 && candidate >= space:
		return candidate - space
	case candidate < reference && reference-candidate > space/2:
		return candidate + space
	}
	return candidate
}

type reader struct {
	data  []byte
	order binary.ByteOrder
	short bool
}

func (r *reader) take(n int) []byte {
	if r.short || n > len(r.data) {
		r.short = true
		return nil
	}
	b := r.data[:n]
	r.data = r.data[n:]
	return b
}

func (r *reader) u8() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return r.order.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return r.order.Uint32(b)
}

func (r *reader) rest() []byte {
	b := r.data
	r.data = nil
	return b
}
