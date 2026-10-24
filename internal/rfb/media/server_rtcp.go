package media

import (
	"encoding/binary"
	"time"

	"github.com/sagernet/sing/common"
)

const (
	rtcpSourceDescription = 202
	rtcpPayloadFeedback   = 206
)

func rewriteExtensionProfile(packet []byte) {
	if packet[0]&0x10 == 0 {
		return
	}
	offset := 12 + 4*int(packet[0]&0x0f)
	profile := binary.BigEndian.Uint16(packet[offset:])
	if profile&appleExtensionMask == appleExtensionProfile {
		binary.BigEndian.PutUint16(packet[offset:], macExtensionProfile(profile))
	}
}

func senderReport(ssrc uint32, ntp uint64, timestamp uint32, packets, octets uint32) []byte {
	packet := make([]byte, 28)
	packet[0] = 0x80
	packet[1] = rtcpSenderReport
	binary.BigEndian.PutUint16(packet[2:], 6)
	binary.BigEndian.PutUint32(packet[4:], ssrc)
	binary.BigEndian.PutUint64(packet[8:], ntp)
	binary.BigEndian.PutUint32(packet[16:], timestamp)
	binary.BigEndian.PutUint32(packet[20:], packets)
	binary.BigEndian.PutUint32(packet[24:], octets)
	return packet
}

type deviceReport struct {
	ntp         uint64
	timestamp   uint32
	receivedAt  time.Time
	description []byte
}

type sentReport struct {
	middle uint32
	at     time.Time
}

func (l *serverLeg) rewriteClientRTCPLocked(compound []byte, now time.Time) (out []byte, keyframe bool) {
	active := l.active
	rtcpPackets(compound, func(packet []byte) {
		switch packet[1] {
		case rtcpFullIntraRequest:
			keyframe = true
		case rtcpPayloadFeedback:
			format := packet[0] & 0x1f
			keyframe = keyframe || format == 1 || format == 4
		case rtcpReceiverReport:
			if active != nil {
				out = l.appendReceiverReportLocked(out, packet, active, now)
			}
		case rtcpApplication:
			if active == nil || len(packet) < 12 {
				return
			}
			start := len(out)
			out = append(out, packet...)
			application := out[start:]
			if active.receiverSSRC != 0 {
				binary.BigEndian.PutUint32(application[4:], active.receiverSSRC)
			}
			if len(application) == 32 && string(application[8:12]) == "RCTL" {
				l.timeClientReportLocked(application)
			}
		}
	})
	return out, keyframe
}

func (l *serverLeg) appendReceiverReportLocked(out []byte, packet []byte, active *serverActive, now time.Time) []byte {
	count := int(packet[0] & 0x1f)
	if len(packet) < 8+24*count {
		return out
	}
	sender := binary.BigEndian.Uint32(packet[4:])
	if active.receiverSSRC != 0 {
		sender = active.receiverSSRC
	}
	start := len(out)
	out = append(out, 0x80, rtcpReceiverReport, 0, 0)
	out = binary.BigEndian.AppendUint32(out, sender)
	blocks := 0
	for i := range count {
		block := packet[8+24*i : 8+24*(i+1)]
		if binary.BigEndian.Uint32(block) != active.ssrc {
			continue
		}
		l.measureClientRTTLocked(binary.BigEndian.Uint32(block[16:]), binary.BigEndian.Uint32(block[20:]), now)
		output := l.outputs[active.ssrc]
		if output == nil || !output.based || output.generation != active.generation {
			continue
		}
		reported := uint16(binary.BigEndian.Uint32(block[8:]))
		index := output.highest - uint64(uint16(output.highest)-reported)
		if index < output.base {
			continue
		}
		out = append(out, block...)
		rewritten := out[len(out)-24:]
		binary.BigEndian.PutUint32(rewritten[8:], uint32(index-output.base)+uint32(output.deviceBase))
		lastReport, delay := l.deviceDelayLocked(active.ssrc, now)
		binary.BigEndian.PutUint32(rewritten[16:], lastReport)
		binary.BigEndian.PutUint32(rewritten[20:], delay)
		blocks++
	}
	out[start] = 0x80 | byte(blocks)
	binary.BigEndian.PutUint16(out[start+2:], uint16((len(out)-start)/4-1))
	return out
}

func (l *serverLeg) measureClientRTTLocked(lastReport, delay uint32, now time.Time) {
	if lastReport == 0 {
		return
	}
	sent := common.Find(l.sentReports[:], func(sent sentReport) bool {
		return sent.middle == lastReport && !sent.at.IsZero()
	})
	if sent.at.IsZero() {
		return
	}
	rtt := now.Sub(sent.at) - time.Duration(float64(delay)/65536*float64(time.Second))
	if rtt >= 0 {
		l.clientRTT = rtt
	}
}

func (l *serverLeg) deviceDelayLocked(ssrc uint32, now time.Time) (uint32, uint32) {
	report := l.deviceReports[ssrc]
	if report == nil || report.receivedAt.IsZero() {
		return 0, 0
	}
	held := max(now.Sub(report.receivedAt)-l.clientRTT, 0)
	return uint32(report.ntp >> 16), uint32(min(held.Seconds()*65536, float64(^uint32(0))))
}

func (l *serverLeg) recordDeviceRTCPLocked(compound []byte, now, arrived time.Time) {
	rtcpPackets(compound, func(packet []byte) {
		switch packet[1] {
		case rtcpSenderReport:
			if len(packet) < 28 {
				return
			}
			ssrc := binary.BigEndian.Uint32(packet[4:])
			report := l.deviceReportLocked(ssrc)
			report.ntp = binary.BigEndian.Uint64(packet[8:])
			report.timestamp = binary.BigEndian.Uint32(packet[16:])
			report.receivedAt = now
			if l.active != nil && ssrc == l.active.ssrc {
				l.timeReportLocked(report.ntp, arrived)
			}
		case rtcpSourceDescription:
			if packet[0]&0x1f != 1 || len(packet) < 8 {
				return
			}
			report := l.deviceReportLocked(binary.BigEndian.Uint32(packet[4:]))
			report.description = append(report.description[:0], packet...)
		}
	})
}

func (l *serverLeg) deviceReportLocked(ssrc uint32) *deviceReport {
	report := l.deviceReports[ssrc]
	if report == nil {
		if len(l.deviceReports) >= maxTrackedVideoSSRC {
			clear(l.deviceReports)
		}
		report = &deviceReport{}
		l.deviceReports[ssrc] = report
	}
	return report
}

func (l *serverLeg) senderReportLocked(now time.Time) ([]byte, bool) {
	active := l.active
	if active == nil || !l.target.IsValid() {
		return nil, false
	}
	output := l.outputs[active.ssrc]
	if output == nil || !output.started {
		return nil, false
	}
	timestamp := output.lastTimestamp + uint32(now.Sub(output.lastSentAt).Seconds()*l.clockRate)
	report := l.deviceReports[active.ssrc]
	if report != nil && !report.receivedAt.IsZero() && now.Sub(report.receivedAt) < 10*time.Second {
		timestamp = report.timestamp + uint32(now.Sub(report.receivedAt).Seconds()*l.clockRate)
	}
	ntp := ntpTime(now)
	packet := senderReport(active.ssrc, ntp, timestamp, output.packets, output.octets)
	if report != nil && len(report.description) > 0 {
		packet = append(packet, report.description...)
	}
	l.sentReports[l.sentNext] = sentReport{middle: uint32(ntp >> 16), at: now}
	l.sentNext = (l.sentNext + 1) % len(l.sentReports)
	return l.reports.appendProtected(nil, packet), true
}
