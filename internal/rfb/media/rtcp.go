package media

import (
	"encoding/binary"
	"math"
	"slices"
	"time"
)

const (
	rtcpFullIntraRequest = 192
	rtcpSenderReport     = 200
	rtcpReceiverReport   = 201
	rtcpApplication      = 204
	maxReportBlocks      = 8
	senderClockWindow    = 30 * time.Second
)

type receptionStats struct {
	started          bool
	baseSequence     uint32
	highestSequence  uint32
	received         uint32
	expectedPrior    uint32
	receivedPrior    uint32
	jitter           float64
	lastTransit      float64
	lastSenderReport uint32
	senderReportAt   time.Time
	lastPacketAt     time.Time
}

func (r *receptionStats) record(sequence uint16, timestamp uint32, clockRate float64, arrival time.Time) uint32 {
	r.lastPacketAt = arrival
	if !r.started {
		r.started = true
		r.baseSequence = uint32(sequence)
		r.highestSequence = uint32(sequence)
		r.received = 1
		r.lastTransit = float64(arrival.UnixNano())/1e9*clockRate - float64(timestamp)
		return 1
	}
	previousHighest := r.highestSequence
	cycles := r.highestSequence &^ 0xffff
	highest := uint16(r.highestSequence)
	if sequenceNewer(sequence, highest) {
		if sequence < highest {
			cycles += 1 << 16
		}
		r.highestSequence = cycles | uint32(sequence)
	}
	r.received++
	transit := float64(arrival.UnixNano())/1e9*clockRate - float64(timestamp)
	difference := math.Abs(transit - r.lastTransit)
	r.lastTransit = transit
	if difference < clockRate*10 {
		r.jitter += (difference - r.jitter) / 16
	}
	return r.highestSequence - previousHighest
}

func (r *receptionStats) appendBlock(out []byte, ssrc uint32, now time.Time) []byte {
	expected := r.highestSequence - r.baseSequence + 1
	lost := int64(expected) - int64(r.received)
	lost = min(max(lost, -0x800000), 0x7fffff)
	expectedInterval := expected - r.expectedPrior
	receivedInterval := r.received - r.receivedPrior
	r.expectedPrior = expected
	r.receivedPrior = r.received
	var fraction uint8
	if expectedInterval > 0 && expectedInterval > receivedInterval {
		fraction = uint8(uint64(expectedInterval-receivedInterval) * 256 / uint64(expectedInterval))
	}
	var delaySinceReport uint32
	if !r.senderReportAt.IsZero() {
		delaySinceReport = uint32(now.Sub(r.senderReportAt).Seconds() * 65536)
	}
	out = binary.BigEndian.AppendUint32(out, ssrc)
	out = binary.BigEndian.AppendUint32(out, uint32(fraction)<<24|uint32(lost)&0xffffff)
	out = binary.BigEndian.AppendUint32(out, r.highestSequence)
	out = binary.BigEndian.AppendUint32(out, uint32(r.jitter))
	out = binary.BigEndian.AppendUint32(out, r.lastSenderReport)
	out = binary.BigEndian.AppendUint32(out, delaySinceReport)
	return out
}

func appendReceiverReport(out []byte, sender uint32, sources map[uint32]*receptionStats, now time.Time) []byte {
	start := len(out)
	out = append(out, make([]byte, 8)...)
	count := 0
	for ssrc, stats := range sources {
		if count == maxReportBlocks || now.Sub(stats.lastPacketAt) > 5*time.Second {
			continue
		}
		out = stats.appendBlock(out, ssrc, now)
		count++
	}
	report := out[start:]
	report[0] = 0x80 | byte(count)
	report[1] = rtcpReceiverReport
	binary.BigEndian.PutUint16(report[2:], uint16(len(report)/4-1))
	binary.BigEndian.PutUint32(report[4:], sender)
	return out
}

// AVConference checks the first RTCP header before SRTCP authentication. With
// reduced-size RTCP disabled, only SR, RR or APP may lead a datagram; a bare FIR
// is rejected (AVConference 2235.63.5.2, macOS 27.0.1 26A434, UUID
// 93D8C8C0-8B9C-3A5E-A47D-01B5FAF73E1B,
// __RTCPParseVCPacket 0x1c0ebf404..0x1c0ebf53c,
// _RTCPReducedSizePackets 0x1c0eb9db4..0x1c0eb9dbc; AVConference 2260.14.1,
// macOS 27.2 26B5101f, UUID EDFB99EF-2F5E-35DD-8E63-089CBD102EFE,
// __RTCPParseVCPacket 0x1c0539ae4..0x1c0539c1c,
// _RTCPReducedSizePackets 0x1c0534494..0x1c053449c).
func appendKeyframeRequest(out []byte, sender uint32, sources map[uint32]*receptionStats, now time.Time) []byte {
	out = appendReceiverReport(out, sender, sources, now)
	// AVConference RTPSendRTCP writes PT 192, length 1 and the sender SSRC when
	// the legacy FIR entry count is zero (AVConference 2235.63.5.2, macOS 27.0.1
	// 26A434, UUID 93D8C8C0-8B9C-3A5E-A47D-01B5FAF73E1B,
	// _RTPSendRTCP 0x1c0eba838..0x1c0eba870; AVConference 2260.14.1, macOS 27.2
	// 26B5101f, UUID EDFB99EF-2F5E-35DD-8E63-089CBD102EFE,
	// _RTPSendRTCP 0x1c0534f18..0x1c0534f50).
	out = append(out, 0x80, rtcpFullIntraRequest, 0, 1)
	return binary.BigEndian.AppendUint32(out, sender)
}

// iShareScreen sends the LTR acknowledgment as APP with a 32-bit value 5
// in the name slot followed by the picture identifier
// (iShareScreen e6db321, src/isharescreen/proxy/protocol/rtcp.py:81-100).
func appendReferenceAcknowledgment(out []byte, sender uint32, order uint16) []byte {
	out = append(out, 0x80, rtcpApplication, 0, 3)
	out = binary.BigEndian.AppendUint32(out, sender)
	out = binary.BigEndian.AppendUint32(out, 5)
	return binary.BigEndian.AppendUint32(out, uint32(order))
}

func rtcpPackets(compound []byte, fn func(packet []byte)) {
	for len(compound) >= 8 {
		length := 4 * (int(binary.BigEndian.Uint16(compound[2:])) + 1)
		if length > len(compound) {
			return
		}
		fn(compound[:length])
		compound = compound[length:]
	}
}

func senderReports(compound []byte, fn func(ssrc uint32, ntpMiddle uint32, timestamp uint32)) {
	rtcpPackets(compound, func(packet []byte) {
		if packet[1] == rtcpSenderReport && len(packet) >= 28 {
			fn(binary.BigEndian.Uint32(packet[4:]), binary.BigEndian.Uint32(packet[10:]), binary.BigEndian.Uint32(packet[16:]))
		}
	})
}

type senderClock struct {
	reports []clockReport
}

type clockReport struct {
	ssrc      uint32
	timestamp uint32
	at        int64
}

func (c *senderClock) observe(ssrc uint32, timestamp uint32, at int64) {
	c.reports = slices.DeleteFunc(c.reports, func(report clockReport) bool {
		return at-report.at > int64(senderClockWindow)
	})
	c.reports = append(c.reports, clockReport{ssrc: ssrc, timestamp: timestamp, at: at})
}

func (c *senderClock) localTime(base uint32, bands int, timestamp uint32) (local int64) {
	for _, report := range c.reports {
		if report.ssrc-base >= uint32(bands) {
			continue
		}
		estimate := report.at - rtpDuration(int64(int32(report.timestamp-timestamp)))
		if local == 0 || estimate < local {
			local = estimate
		}
	}
	return
}

type rateFeedback struct {
	epoch           time.Time
	haveSample      bool
	sampleTimestamp uint32
	sampleArrival   time.Time
	sampleReceived  uint32
	received        uint32
	delay           owrdEstimator
	burstLoss       uint32
	highest         map[uint32]uint16
	videoArrived    bool
}

func newRateFeedback(now time.Time) rateFeedback {
	return rateFeedback{epoch: now, highest: make(map[uint32]uint16)}
}

func (f *rateFeedback) record(ssrc uint32, sequence uint16, timestamp uint32, arrival time.Time) {
	if highest, seen := f.highest[ssrc]; !seen {
		if len(f.highest) >= maxTrackedVideoSSRC {
			clear(f.highest)
		}
		f.highest[ssrc] = sequence
	} else if step := sequence - highest; step != 0 && step <= 0x7ffe {
		f.burstLoss = max(f.burstLoss, uint32(step)-1)
		f.highest[ssrc] = sequence
	}
	if !f.haveSample || f.sampleTimestamp-timestamp > 0x7ffffffe {
		f.haveSample = true
		f.sampleTimestamp = timestamp
		f.sampleArrival = arrival
		f.sampleReceived = f.received
		milliseconds := uint32(uint64(arrival.Sub(f.epoch).Milliseconds()))
		f.delay.sample(timestamp, videoClockRate, milliseconds)
	}
	f.received++
}

func (f *rateFeedback) restart() {
	*f = newRateFeedback(f.epoch)
}

func (f *rateFeedback) appendReport(out []byte, sender uint32, now time.Time) ([]byte, bool) {
	if !f.haveSample || !f.videoArrived {
		return out, false
	}
	// AVConference sendRateControlFeedback passes serializer configuration bytes
	// {2,1,1,1,0,0}, shifts the echo timestamp right by 8 when
	// _useLowPrecisionEchoTimeStamp is set, and counts the clock in 1024 ticks
	// per second. For echo=0x1234, loss=3, count=0x567, arrival=126, clock=129
	// and packed delay=1024 the serializer writes
	// 85000004123400000000007e0081040035670000 (AVConference 2235.63.5.2,
	// macOS 27.0.1 26A434, UUID 93D8C8C0-8B9C-3A5E-A47D-01B5FAF73E1B,
	// -[VCVideoStreamRateAdaptationFeedbackOnly sendRateControlFeedback] 0x1c1189ec8..0x1c118a2a0,
	// _VCMediaControlInfoSerializeWithData 0x1c13fc464..0x1c13fc780;
	// AVConference 2260.14.1, macOS 27.2 26B5101f, UUID
	// EDFB99EF-2F5E-35DD-8E63-089CBD102EFE,
	// -[VCVideoStreamRateAdaptationFeedbackOnly sendRateControlFeedback] 0x1c0806670..0x1c0806a48,
	// _VCMediaControlInfoSerializeWithData 0x1c0a795a0..0x1c0a798bc).
	start := len(out)
	out = append(out, make([]byte, 32)...)
	report := out[start:]
	copy(report, []byte{0x80, rtcpApplication, 0, 7})
	binary.BigEndian.PutUint32(report[4:], sender)
	copy(report[8:], "RCTL")
	copy(report[12:], []byte{0x85, 0, 0, 4})
	binary.BigEndian.PutUint16(report[16:], uint16(f.sampleTimestamp>>8))
	sinceArrival := min(max(now.Sub(f.sampleArrival)/time.Millisecond, 0), math.MaxUint16)
	binary.BigEndian.PutUint16(report[22:], uint16(sinceArrival))
	clock := int64(now.Sub(f.epoch).Seconds() * 1024)
	binary.BigEndian.PutUint16(report[24:], uint16(clock))
	binary.BigEndian.PutUint16(report[26:], uint16(min(saturatingUint32(f.delay.value*8192), math.MaxUint16)))
	binary.BigEndian.PutUint16(report[28:], uint16(min(f.burstLoss, 15)<<12|f.sampleReceived&0xfff))
	f.burstLoss = 0
	return out, true
}
