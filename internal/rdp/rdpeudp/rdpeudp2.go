package rdpeudp

import (
	"encoding/binary"
	"io"
	"slices"
	"time"

	"github.com/sagernet/sing/common"
)

const (
	packetACK          = 0x001
	packetData         = 0x004
	packetAckVector    = 0x008
	packetAckOfAcks    = 0x010
	packetOverheadSize = 0x040
	packetDelayAckInfo = 0x100
	packetReset        = 0x200
	packetFlagsMask    = 0x0fff
	packetWindowShift  = 12

	prefixPosition    = 7
	prefixTypeDummy   = 8
	shortPacketLength = 7

	// rdpnanoTransport.dll of Windows 11 26200 numbers the packets it sends from 100 in both roles, and
	// starts its receive window at 2 until an AckOfAcks lower bound moves it.
	initialPacketSeq = 100
	logPacketWindow  = 12
	packetWindow     = 1 << logPacketWindow

	ackPayloadLength  = 7
	maxDelayedAcks    = 15
	maxAckVector      = 21
	bitmapLength      = 7
	maxRunLength2     = 63
	invalidAckTimeGap = 255
	timestampUnit     = 4 * time.Microsecond
	timestampMask     = 1<<24 - 1
	dataOverhead2     = 1 + 2 + max(ackPayloadLength+maxDelayedAcks, 3+4+maxAckVector) + 2 + 2 + 2

	// rdpnanoTransport.dll of Windows 11 26200 keeps a connection alive with a dummy packet of four
	// zero bytes, and closes it with one of eight zero bytes followed by 1.
	keepAliveBody = 4
	finPacket     = 1
)

func (c *Conn) connectUDP2() {
	// rdpbase.dll of Windows 11 26200 takes the first datagram after the SYN+ACK as the first
	// MS-RDPEUDP2 packet, so there is no final ACK.
	c.handshake = nil
	c.nextSeq = 1
	c.nextOrder = initialPacketSeq
	c.receiveBase = initialPacketSeq
	c.receiveHighest = initialPacketSeq - 1
	c.nextDelivery = 1
	c.ackEvery = 1
}

func (c *Conn) receivePacket(datagram []byte, now time.Time) {
	datagram[0], datagram[prefixPosition] = datagram[prefixPosition], datagram[0]
	prefix := datagram[0]
	packet := datagram[1:]
	if length := int(prefix >> 5); length < shortPacketLength {
		packet = packet[:length]
	}
	dummy := prefix>>1&0x0f == prefixTypeDummy
	r := &reader{data: packet, order: binary.LittleEndian}
	header := r.u16()
	flags := header & packetFlagsMask
	var ackSeq uint16
	var ackGap byte
	if flags&packetACK != 0 {
		ackSeq = r.u16()
		r.take(3)
		ackGap = r.u8()
		r.take(int(r.u8() & 0x0f))
	}
	if flags&packetOverheadSize != 0 {
		r.u8()
	}
	var delayedAcks byte
	var delayedAckTimeout uint16
	if flags&packetDelayAckInfo != 0 {
		delayedAcks = r.u8()
		delayedAckTimeout = r.u16()
	}
	var lowerBound uint16
	if flags&packetAckOfAcks != 0 {
		lowerBound = r.u16()
	}
	var dataSeq uint16
	if flags&packetData != 0 {
		dataSeq = r.u16()
	}
	var vectorBase uint16
	var vector []byte
	vectorGap := byte(invalidAckTimeGap)
	if flags&packetAckVector != 0 {
		vectorBase = r.u16()
		size := r.u8()
		if size&0x80 != 0 {
			r.take(3)
			vectorGap = r.u8()
		}
		vector = r.take(int(size & 0x7f))
	}
	// rdpnanoTransport.dll of Windows 11 26200 sets flag 0x200 on packets without acknowledgments to
	// announce sequence resets, follows the acknowledgments of a packet with the flag with the 64-bit
	// sequence number its receive window was reset to, and closes the connection instead when the peer
	// never announced them.
	if flags&packetReset != 0 && flags&(packetACK|packetAckVector) != 0 {
		r.take(8)
	}
	var channelSeq uint16
	var payload []byte
	if flags&packetData != 0 {
		if !dummy {
			channelSeq = r.u16()
		}
		payload = r.rest()
	}
	if r.short {
		return
	}
	c.receivedPackets++
	c.remoteWindow = 1 << (header >> packetWindowShift)
	if flags&packetACK != 0 {
		c.receiveAckPayload(ackSeq, ackGap, now)
	}
	if flags&packetAckVector != 0 {
		c.receiveAckVector(vectorBase, vector, vectorGap, now)
	}
	if flags&packetDelayAckInfo != 0 {
		c.ackEvery = min(int(delayedAcks), maxDelayedAcks) + 1
		c.ackTimeout = time.Duration(delayedAckTimeout) * time.Millisecond
	}
	if flags&packetAckOfAcks != 0 {
		c.receiveLowerBound(lowerBound)
	}
	if flags&packetData != 0 {
		c.receiveDataPayload(dataSeq, channelSeq, payload, dummy, now)
	}
}

func (c *Conn) oldestPacket() uint64 {
	oldest := c.nextOrder
	for _, p := range c.inflight {
		oldest = min(oldest, p.order)
	}
	return oldest
}

func (c *Conn) receiveAckPayload(wire uint16, gap byte, now time.Time) {
	seq := reconstruct(uint64(wire), 16, c.nextOrder-1)
	if seq >= c.nextOrder {
		return
	}
	c.peerBase = max(c.peerBase, seq+1)
	newest := c.acknowledge(func(p *outgoing) bool { return p.order <= seq })
	if newest != nil && newest.order == seq && gap != invalidAckTimeGap {
		c.sample(max(now.Sub(newest.sent)-time.Duration(gap)*time.Millisecond, 0))
	}
	c.detectLoss(now)
}

func (c *Conn) receiveAckVector(wireBase uint16, vector []byte, gap byte, now time.Time) {
	base := reconstruct(uint64(wireBase), 16, c.nextOrder-1)
	if base > c.nextOrder {
		return
	}
	var received []uint64
	seq := base
	for _, entry := range vector {
		if entry&0x80 != 0 {
			length := uint64(entry & 0x3f)
			for i := range length {
				if entry&0x40 != 0 {
					received = append(received, seq+i)
				}
			}
			seq += length
			continue
		}
		for i := range uint64(bitmapLength) {
			if entry>>i&1 != 0 {
				received = append(received, seq+i)
			}
		}
		seq += bitmapLength
	}
	c.peerBase = max(c.peerBase, base)
	newest := c.acknowledge(func(p *outgoing) bool { return p.order < base || slices.Contains(received, p.order) })
	if newest != nil && len(received) > 0 && newest.order == received[len(received)-1] && gap != invalidAckTimeGap {
		c.sample(max(now.Sub(newest.sent)-time.Duration(gap)*time.Millisecond, 0))
	}
	c.detectLoss(now)
}

func (c *Conn) receiveLowerBound(wire uint16) {
	seq := reconstruct(uint64(wire), 16, c.receiveHighest)
	if seq <= c.receiveBase {
		return
	}
	if seq > c.receiveHighest {
		c.sequencedPackets += seq - 1 - c.receiveHighest
		c.missedPackets += seq - 1 - c.receiveHighest
		c.receiveState = c.receiveState[:0]
		c.receiveHighest = seq - 1
	} else {
		c.receiveState = c.receiveState[seq-c.receiveBase:]
	}
	c.receiveBase = seq
	c.arrivals = slices.DeleteFunc(c.arrivals, func(a arrival) bool { return a.seq < seq })
}

func (c *Conn) receiveDataPayload(wireSeq uint16, wireChannelSeq uint16, payload []byte, dummy bool, now time.Time) {
	seq := reconstruct(uint64(wireSeq), 16, c.receiveHighest)
	if seq < c.receiveBase {
		return
	}
	index := seq - c.receiveBase
	if index >= packetWindow || index < uint64(len(c.receiveState)) && c.receiveState[index] {
		return
	}
	channelSeq := reconstruct(uint64(wireChannelSeq), 16, c.nextDelivery)
	if !dummy && channelSeq >= c.nextDelivery+packetWindow {
		return
	}
	for uint64(len(c.receiveState)) <= index {
		c.receiveState = append(c.receiveState, false)
	}
	c.receiveState[index] = true
	if seq != c.receiveHighest+1 {
		c.ackNow = true
	}
	if seq > c.receiveHighest {
		c.sequencedPackets += seq - c.receiveHighest
		c.missedPackets += seq - c.receiveHighest - 1
		c.receiveHighest = seq
		c.highestArrival = now
	}
	c.arrivals = append(c.arrivals, arrival{seq: seq, at: now})
	if c.unacknowledged == 0 {
		c.ackAt = now.Add(c.ackDelay())
	}
	c.unacknowledged++
	if !dummy {
		c.deliver(channelSeq, payload)
	} else if len(payload) > 0 && payload[len(payload)-1] == finPacket {
		c.fail(io.EOF)
	}
}

func (c *Conn) sendPacket(p *outgoing, now time.Time) {
	var ack, vector []byte
	// rdpnanoTransport.dll of Windows 11 26200 declares every hole of a repeated ACK vector lost at
	// once, so acknowledgments only go out with new arrivals.
	if c.unacknowledged > 0 {
		ack, vector = c.acknowledgment(now)
	}
	var flags uint16
	b := make([]byte, 3, mtu)
	if ack != nil {
		flags |= packetACK
		b = append(b, ack...)
	}
	if lowest := c.oldestPacket(); lowest > c.peerBase {
		flags |= packetAckOfAcks
		b = binary.LittleEndian.AppendUint16(b, uint16(lowest))
	}
	dummy := p == nil && ack == nil && vector == nil
	if dummy {
		flags |= packetData
		b = binary.LittleEndian.AppendUint16(b, uint16(c.nextOrder))
		c.nextOrder++
	} else if p != nil {
		flags |= packetData
		b = binary.LittleEndian.AppendUint16(b, uint16(p.order))
	}
	if vector != nil {
		flags |= packetAckVector
		b = append(b, vector...)
	}
	var prefix byte
	if dummy {
		b = append(b, make([]byte, keepAliveBody)...)
		prefix = prefixTypeDummy << 1
	} else if p != nil {
		b = binary.LittleEndian.AppendUint16(b, uint16(p.seq))
		b = append(b, p.data...)
	}
	binary.LittleEndian.PutUint16(b[1:], flags|logPacketWindow<<packetWindowShift)
	length := min(len(b)-1, shortPacketLength)
	for len(b) < 1+shortPacketLength {
		b = append(b, 0)
	}
	b[0] = prefix | byte(length<<5)
	b[0], b[prefixPosition] = b[prefixPosition], b[0]
	c.write(b)
}

func (c *Conn) acknowledgment(now time.Time) (ack []byte, vector []byte) {
	gap := byte(min(now.Sub(c.highestArrival).Milliseconds(), invalidAckTimeGap-1))
	missing := slices.Index(c.receiveState, false)
	if missing < 0 {
		common.SortBy(c.arrivals, func(a arrival) uint64 { return a.seq })
		var gaps []time.Duration
		if last := len(c.arrivals) - 1; last >= 0 && c.arrivals[last].seq == c.receiveHighest {
			for i := last; i > 0 && len(gaps) < maxDelayedAcks && c.arrivals[i-1].seq+1 == c.arrivals[i].seq; i-- {
				gaps = append(gaps, max(c.arrivals[i].at.Sub(c.arrivals[i-1].at), 0))
			}
		}
		var scale uint
		for slices.ContainsFunc(gaps, func(g time.Duration) bool { return g.Microseconds()>>scale > 0xff }) && scale < 0x0f {
			scale++
		}
		ack = binary.LittleEndian.AppendUint16(nil, uint16(c.receiveHighest))
		ack = c.appendTimestamp(ack, c.highestArrival)
		ack = append(ack, gap, byte(scale<<4|uint(len(gaps))))
		for _, g := range gaps {
			ack = append(ack, byte(min(g.Microseconds()>>scale, 0xff)))
		}
		c.receiveBase = c.receiveHighest + 1
		c.receiveState = c.receiveState[:0]
		c.arrivals = c.arrivals[:0]
		return ack, nil
	}
	c.receiveState = c.receiveState[missing:]
	c.receiveBase += uint64(missing)
	c.arrivals = c.arrivals[:0]
	entries, complete := ackVectorEntries(c.receiveState)
	vector = binary.LittleEndian.AppendUint16(nil, uint16(c.receiveBase))
	if !complete {
		vector = append(vector, byte(len(entries)))
		return nil, append(vector, entries...)
	}
	vector = append(vector, 0x80|byte(len(entries)))
	vector = c.appendTimestamp(vector, c.highestArrival)
	vector = append(vector, gap)
	return nil, append(vector, entries...)
}

func (c *Conn) appendTimestamp(b []byte, at time.Time) []byte {
	ts := uint32(at.Sub(c.start)/timestampUnit) & timestampMask
	return append(b, byte(ts), byte(ts>>8), byte(ts>>16))
}

func ackVectorEntries(state []bool) ([]byte, bool) {
	var entries []byte
	i := 0
	for i < len(state) && len(entries) < maxAckVector {
		run := 1
		for i+run < len(state) && state[i+run] == state[i] && run < maxRunLength2 {
			run++
		}
		if run > bitmapLength {
			entry := byte(0x80 | run)
			if state[i] {
				entry |= 0x40
			}
			entries = append(entries, entry)
			i += run
			continue
		}
		var entry byte
		for bit := range bitmapLength {
			if i+bit < len(state) && state[i+bit] {
				entry |= 1 << bit
			}
		}
		entries = append(entries, entry)
		i += bitmapLength
	}
	return entries, i >= len(state)
}
