package rdpeudp

import (
	"encoding/binary"
	"slices"
	"time"
)

const (
	flagData       = 0x0008
	flagFEC        = 0x0010
	flagCN         = 0x0020
	flagCWR        = 0x0040
	flagAckOfAcks  = 0x0100
	flagAckDelayed = 0x0400

	sourceHeaderLength = 8
	ackOfAcksLength    = 4
	vectorReceived     = 0
	vectorMissing      = 3
	maxRunLength       = 64
	dataOverhead       = headerLength + 2 + maxVectorEntries + 2 + ackOfAcksLength + sourceHeaderLength

	// rdpbase.dll of Windows 11 26200 drops a whole datagram whose ACK vector covers more than 2050
	// datagrams.
	maxVectorEntries = 32

	history           = 1024
	maxAhead          = 4 * history
	ackOfAcksInterval = 20
	ackPrompt         = 5 * time.Millisecond
	ackEvery          = 2
)

func (c *Conn) connectUDP1(receiveWindow uint16) {
	// rdpbase.dll of Windows 11 26200 sets up its stream asynchronously after the final ACK and
	// drops datagrams until then, but keeps the data carried by the final ACK itself, so the first
	// data datagram doubles as the final ACK and this one only answers a retransmitted SYN+ACK.
	c.handshake = binary.BigEndian.AppendUint32(nil, c.remoteISN)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, window)
	c.handshake = binary.BigEndian.AppendUint16(c.handshake, flagACK)
	c.handshake = append(c.handshake, 0, 1, 0, 0)
	c.nextSeq = uint64(c.localISN) + 1
	c.nextOrder = uint64(c.localISN) + 1
	c.nextCoded = c.localISN + 1
	c.receiveBase = uint64(c.remoteISN) + 1
	c.receiveHighest = uint64(c.remoteISN)
	c.nextDelivery = uint64(c.remoteISN) + 1
	c.remoteWindow = max(int(receiveWindow), 1)
	c.ackEvery = ackEvery
}

func (c *Conn) receiveDatagram(datagram []byte, now time.Time) {
	sourceAck := binary.BigEndian.Uint32(datagram)
	receiveWindow := binary.BigEndian.Uint16(datagram[4:])
	flags := binary.BigEndian.Uint16(datagram[6:])
	r := &reader{data: datagram[headerLength:], order: binary.BigEndian}
	var vector []byte
	if flags&flagACK != 0 {
		size := int(r.u16())
		vector = r.take(size)
		r.take(padding(size))
	}
	var reset uint32
	if flags&flagAckOfAcks != 0 {
		reset = r.u32()
	}
	source := flags&flagData != 0 && flags&flagFEC == 0
	var sourceStart uint32
	var payload []byte
	if source {
		r.take(4)
		sourceStart = r.u32()
		payload = r.rest()
	}
	if r.short {
		return
	}
	c.receivedPackets++
	c.remoteWindow = max(int(receiveWindow), 1)
	if flags&flagACK != 0 {
		c.receiveAck(sourceAck, vector, flags&flagAckDelayed != 0, now)
	}
	if flags&flagCN != 0 {
		c.congestionNotified()
	}
	if flags&flagAckOfAcks != 0 {
		c.receiveAckOfAcks(reset)
	}
	if source {
		c.receiveData(sourceStart, payload, now)
	}
}

func (c *Conn) receiveAck(sourceAck uint32, vector []byte, delayed bool, now time.Time) {
	highest := reconstruct(uint64(sourceAck), 32, c.nextSeq-1)
	if highest >= c.nextSeq {
		return
	}
	lowest := c.oldestSource()
	type span struct{ low, high uint64 }
	var received []span
	top := highest
	for _, element := range vector {
		if top < lowest {
			break
		}
		// rdpbase.dll of Windows 11 26200 encodes and decodes a run of n datagrams as n - 1, unlike
		// the example in MS-RDPEUDP 4.2.1.
		length := uint64(element&0x3f) + 1
		if element>>6 == vectorReceived {
			received = append(received, span{top - min(length-1, top), top})
		}
		if length > top {
			break
		}
		top -= length
	}
	acknowledged := func(p *outgoing) bool {
		return slices.ContainsFunc(received, func(s span) bool { return p.seq >= s.low && p.seq <= s.high })
	}
	newest := c.acknowledge(acknowledged)
	c.lost = slices.DeleteFunc(c.lost, acknowledged)
	if newest != nil && newest.transmits == 1 && !delayed {
		c.sample(now.Sub(newest.sent))
	}
	c.detectLoss(now)
}

func (c *Conn) oldestSource() uint64 {
	oldest := c.nextSeq
	for _, p := range c.inflight {
		oldest = min(oldest, p.seq)
	}
	for _, p := range c.lost {
		oldest = min(oldest, p.seq)
	}
	return oldest
}

func (c *Conn) congestionNotified() {
	c.congestionReduced = true
	if c.highestAckedOrder < c.recovery {
		return
	}
	c.recovery = c.nextOrder
	c.reduce()
}

func (c *Conn) receiveAckOfAcks(wire uint32) {
	reset := reconstruct(uint64(wire), 32, c.receiveHighest)
	if reset > c.receiveHighest || reset < c.receiveBase {
		return
	}
	c.receiveState = c.receiveState[reset+1-c.receiveBase:]
	c.receiveBase = reset + 1
}

func (c *Conn) receiveData(sourceStart uint32, payload []byte, now time.Time) {
	seq := reconstruct(uint64(sourceStart), 32, c.receiveHighest)
	if seq < c.receiveBase {
		c.ackNow = true
		return
	}
	index := seq - c.receiveBase
	if index >= maxAhead {
		return
	}
	if index < uint64(len(c.receiveState)) && c.receiveState[index] {
		c.ackNow = true
		return
	}
	if seq > c.nextDelivery && len(c.reorder) >= window {
		return
	}
	previous := len(c.receiveState)
	for uint64(len(c.receiveState)) <= index {
		c.receiveState = append(c.receiveState, false)
	}
	c.receiveState[index] = true
	if ackVectorLength(c.receiveState) > maxVectorEntries {
		// rdpbase.dll of Windows 11 26200 takes every datagram older than the oldest one an ACK
		// vector covers as received, so a missing datagram must never fall out of the vector.
		c.receiveState[index] = false
		c.receiveState = c.receiveState[:previous]
		return
	}
	if seq != c.receiveHighest+1 {
		c.ackNow = true
	}
	if seq > c.receiveHighest {
		c.sequencedPackets += seq - c.receiveHighest
		c.missedPackets += seq - c.receiveHighest - 1
		c.receiveHighest = seq
	}
	if c.unacknowledged == 0 {
		c.ackSince = now
		c.ackAt = now.Add(c.ackDelay())
	}
	c.unacknowledged++
	if extra := len(c.receiveState) - history; extra > 0 {
		released := 0
		for released < extra && c.receiveState[released] {
			released++
		}
		c.receiveState = c.receiveState[released:]
		c.receiveBase += uint64(released)
	}
	c.deliver(seq, payload)
}

func (c *Conn) sendDatagram(p *outgoing, now time.Time) {
	flags := uint16(flagACK)
	if c.unacknowledged == 0 || now.Sub(c.ackSince) > ackPrompt {
		flags |= flagAckDelayed
	}
	if p != nil {
		flags |= flagData
		if c.congestionReduced {
			flags |= flagCWR
		}
	}
	c.sinceAckOfAcks++
	reset := c.oldestSource() - 1
	ackOfAcks := c.sinceAckOfAcks >= ackOfAcksInterval && reset > c.ackOfAcksSent && reset > uint64(c.localISN)
	if ackOfAcks {
		flags |= flagAckOfAcks
		c.sinceAckOfAcks = 0
		c.ackOfAcksSent = reset
	}
	b := binary.BigEndian.AppendUint32(make([]byte, 0, mtu), uint32(c.receiveHighest))
	b = binary.BigEndian.AppendUint16(b, window)
	b = binary.BigEndian.AppendUint16(b, flags)
	b = c.appendAckVector(b)
	if ackOfAcks {
		b = binary.BigEndian.AppendUint32(b, uint32(reset))
	}
	if p != nil {
		b = binary.BigEndian.AppendUint32(b, c.nextCoded)
		b = binary.BigEndian.AppendUint32(b, uint32(p.seq))
		b = append(b, p.data...)
		c.nextCoded++
		c.congestionReduced = false
	}
	c.write(b)
}

func (c *Conn) appendAckVector(b []byte) []byte {
	var elements []byte
	for i := len(c.receiveState) - 1; i >= 0 && len(elements) < maxVectorEntries; {
		j := ackRun(c.receiveState, i)
		element := byte(vectorMissing << 6)
		if c.receiveState[i] {
			element = vectorReceived << 6
		}
		elements = append(elements, element|byte(i-j-1))
		i = j
	}
	if len(elements) == 0 {
		elements = append(elements, vectorReceived<<6)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(elements)))
	b = append(b, elements...)
	return append(b, make([]byte, padding(len(elements)))...)
}

func ackRun(state []bool, i int) int {
	j := i
	for j >= 0 && state[j] == state[i] && i-j < maxRunLength {
		j--
	}
	return j
}

func ackVectorLength(state []bool) int {
	oldest := slices.Index(state, false)
	if oldest < 0 {
		return min(len(state), 1)
	}
	var length int
	for i := len(state) - 1; i >= oldest; i = ackRun(state, i) {
		length++
	}
	return length
}

func padding(size int) int {
	return (4 - (2+size)%4) % 4
}
