import AVFoundation
import Atomics
import Foundation
import QuartzCore

nonisolated final class AudioPlayer: @unchecked Sendable {
    private let queue = DispatchQueue(label: "io.nekohasekai.screensharing.audio")
    private var engine: AVAudioEngine?
    private let inputFormat: AVAudioFormat
    private let outputFormat: AVAudioFormat
    private let converter: AVAudioConverter
    private let ring: AudioRing
    private let output: AudioOutput
    private let packetFrames: Int
    private let sampleRate: Double
    private let fadeFrames: Int
    private var started = false
    private var foreground = false
    private var blocked = false
    private var previous: AVAudioPCMBuffer?
    private var undecoded = 0
    private var origin: CFTimeInterval?
    private var lastTimestamp: Int64 = 0
    private var lastArrival: CFTimeInterval = 0
    private var mediaFrames: Int64 = 0
    private var lastLag = 0.0
    private var lags: [Double] = []
    private var nextLag = 0
    private var earliestLag = 0.0
    private var unreviewed = 0
    private var target = AudioPlayer.minimumTarget
    private var lastSound = -CFTimeInterval.infinity
    private var quietTimer: DispatchSourceTimer?
    private var observers: [any NSObjectProtocol] = []
    private let failed: @Sendable (any Error) -> Void
    private let stopped: @Sendable () -> Void

    private static let minimumTarget = 0.015
    private static let maximumTarget = 0.25
    private static let lagWindow = 1500
    private static let reviewInterval = 25
    private static let targetDecay = 0.003
    private static let maximumFilledLoss = 0.11
    private static let fadedPackets = 3
    private static let minimumPause = 0.05
    private static let maximumPause = 600.0
    private static let restartJump = 1.0
    private static let quietLimit = 60.0
    private static let audibleLevel: Float = 1 / 32768

    init(format: AudioFormat, failed: @escaping @Sendable (any Error) -> Void, stopped: @escaping @Sendable () -> Void) throws {
        self.failed = failed
        self.stopped = stopped
        let pcm = format.formatID == kAudioFormatLinearPCM
        var description = AudioStreamBasicDescription(
            mSampleRate: format.sampleRate, mFormatID: kAudioFormatMPEG4AAC_ELD, mFormatFlags: 0, mBytesPerPacket: 0,
            mFramesPerPacket: format.framesPerPacket, mBytesPerFrame: 0, mChannelsPerFrame: format.channels, mBitsPerChannel: 0, mReserved: 0
        )
        let name = "\(Int(format.sampleRate)) Hz \(format.channels)-channel \(pcm ? "16-bit PCM" : "AAC-ELD")"
        guard
            let inputFormat = pcm
                ? AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: format.sampleRate, channels: format.channels, interleaved: true)
                : AVAudioFormat(streamDescription: &description),
            let outputFormat = AVAudioFormat(standardFormatWithSampleRate: format.sampleRate, channels: format.channels)
        else { throw MessageError("AVAudioFormat rejected \(name)") }
        guard let converter = AVAudioConverter(from: inputFormat, to: outputFormat) else {
            throw MessageError("AVAudioConverter can't decode \(name)")
        }
        if !pcm {
            converter.magicCookie = format.magicCookie
        }
        self.inputFormat = inputFormat
        self.outputFormat = outputFormat
        self.converter = converter
        packetFrames = Int(format.framesPerPacket)
        sampleRate = format.sampleRate
        fadeFrames = AudioOutput.fadeFrames(sampleRate: format.sampleRate)
        ring = AudioRing(channels: Int(format.channels), frames: Int(2 * format.sampleRate))
        ring.target.store(Int(Self.minimumTarget * format.sampleRate), ordering: .relaxed)
        output = AudioOutput(ring: ring, sampleRate: format.sampleRate)
        #if os(iOS)
        observers.append(
            NotificationCenter.default.addObserver(
                forName: AVAudioSession.interruptionNotification, object: AVAudioSession.sharedInstance(), queue: nil
            ) { [weak self] notification in
                switch (notification.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt).flatMap(AVAudioSession.InterruptionType.init) {
                case .began: self?.interrupt()
                case .ended: self?.resume()
                default: break
                }
            })
        #endif
    }

    deinit {
        for observer in observers {
            NotificationCenter.default.removeObserver(observer)
        }
        engine?.stopForRelease()
    }

    func stop() {
        queue.async { [self] in
            foreground = false
            halt()
        }
    }

    var isPlaying: Bool {
        queue.sync { started && engine?.isRunning == true }
    }

    func setInBackground(_ background: Bool) {
        queue.async { [self] in
            foreground = !background
            if !background {
                blocked = false
            }
        }
    }

    private func play() {
        guard !started else { return }
        do {
            #if os(iOS)
            try RemoteAudioSession.activate()
            #endif
            let engine = self.engine ?? AVAudioEngine()
            if self.engine == nil {
                let output = output
                let source = AVAudioSourceNode(format: outputFormat) { isSilence, _, frameCount, buffers in
                    if output.render(Int(frameCount), into: UnsafeMutableAudioBufferListPointer(buffers)) {
                        isSilence.pointee = true
                    }
                    return noErr
                }
                engine.attach(source)
                engine.connect(source, to: engine.mainMixerNode, format: outputFormat)
                observers.append(
                    NotificationCenter.default.addObserver(forName: .AVAudioEngineConfigurationChange, object: engine, queue: nil) {
                        [weak self] _ in
                        self?.restart()
                    })
                self.engine = engine
            }
            try engine.start()
            started = true
            stopWhenQuiet()
        } catch {
            blocked = true
            failed(error)
            stopped()
        }
    }

    private func restart() {
        queue.async { [self] in
            guard started else { return }
            // AVAudioEngineConfigurationChange can arrive while the engine still reports running, although its documentation
            // says the engine stops itself first (iOS 26.4.2, https://developer.apple.com/forums/thread/825253).
            engine?.stop()
            started = false
            ring.flushRequested.store(true, ordering: .relaxed)
            play()
        }
    }

    private func interrupt() {
        queue.async { [self] in
            blocked = true
            guard started else { return }
            ring.flushRequested.store(true, ordering: .relaxed)
            halt()
        }
    }

    private func resume() {
        queue.async { [self] in blocked = false }
    }

    private func halt() {
        quietTimer?.cancel()
        quietTimer = nil
        guard started else { return }
        engine?.stop()
        started = false
        stopped()
    }

    private func stopWhenQuiet() {
        let quiet = CACurrentMediaTime() - lastSound
        guard quiet < Self.quietLimit else {
            halt()
            return
        }
        if quietTimer == nil {
            let timer = DispatchSource.makeTimerSource(queue: queue)
            timer.setEventHandler { [weak self] in self?.stopWhenQuiet() }
            timer.resume()
            quietTimer = timer
        }
        quietTimer?.schedule(deadline: .now() + (Self.quietLimit - quiet))
    }

    func enqueue(_ unit: AudioAccessUnit) {
        let arrival = CACurrentMediaTime()
        queue.async { [self] in
            measure(arrival, timestamp: unit.timestamp, lost: unit.lost)
            let lost = unit.lost + undecoded
            guard let pcm = decode(unit.data) else {
                undecoded = lost + 1
                return
            }
            undecoded = 0
            defer { previous = pcm }
            if isAudible(pcm) {
                lastSound = arrival
                if foreground, !blocked {
                    play()
                }
            }
            guard started else { return }
            if lost > 0 {
                conceal(lost, before: pcm)
            }
            write(pcm, frames: Int(pcm.frameLength))
        }
    }

    private func isAudible(_ buffer: AVAudioPCMBuffer) -> Bool {
        guard let channels = buffer.floatChannelData else { return false }
        let frames = Int(buffer.frameLength)
        for channel in 0..<Int(buffer.format.channelCount) {
            if UnsafeBufferPointer(start: channels[channel], count: frames).contains(where: { abs($0) > Self.audibleLevel }) {
                return true
            }
        }
        return false
    }

    private func measure(_ arrival: CFTimeInterval, timestamp: Int64, lost: Int) {
        guard let origin else {
            restartMeasuring(at: arrival, timestamp: timestamp)
            return
        }
        let counted = Int64(lost + 1) * Int64(packetFrames)
        let step = timestamp - lastTimestamp
        let pause = Double(step - counted) / sampleRate
        let paused = pause > Self.minimumPause && pause < Self.maximumPause && arrival - lastArrival > pause / 2
        lastTimestamp = timestamp
        lastArrival = arrival
        mediaFrames += paused ? step : counted
        let lag = arrival - origin - Double(mediaFrames) / sampleRate
        guard abs(lag - lastLag) < Self.restartJump else {
            restartMeasuring(at: arrival, timestamp: timestamp)
            return
        }
        lastLag = lag
        if lags.count < Self.lagWindow {
            lags.append(lag)
        } else {
            lags[nextLag] = lag
        }
        nextLag = (nextLag + 1) % Self.lagWindow
        earliestLag = min(earliestLag, lag)
        target = max(target, min(lag - earliestLag + Self.minimumTarget, Self.maximumTarget))
        unreviewed += 1
        if unreviewed >= Self.reviewInterval {
            unreviewed = 0
            earliestLag = lags.min() ?? lag
            let wanted = min((lags.max() ?? lag) - earliestLag + Self.minimumTarget, Self.maximumTarget)
            let decay = Self.targetDecay * Double(Self.reviewInterval * packetFrames) / sampleRate
            target = max(wanted, target - decay)
        }
        ring.target.store(Int(target * sampleRate), ordering: .relaxed)
    }

    private func restartMeasuring(at arrival: CFTimeInterval, timestamp: Int64) {
        origin = arrival
        lastTimestamp = timestamp
        lastArrival = arrival
        mediaFrames = 0
        lastLag = 0
        lags.removeAll(keepingCapacity: true)
        nextLag = 0
        earliestLag = 0
        unreviewed = 0
    }

    private func conceal(_ lost: Int, before next: AVAudioPCMBuffer) {
        guard let previous, let source = previous.floatChannelData, let destination = next.floatChannelData else { return }
        let length = Int(previous.frameLength)
        guard length > 1 else { return }
        let lostFrames = lost * packetFrames
        let filled = Double(lostFrames) / sampleRate <= max(Self.maximumFilledLoss, target) ? lostFrames : 0
        let joined = min(fadeFrames, Int(next.frameLength))
        guard let filler = AVAudioPCMBuffer(pcmFormat: outputFormat, frameCapacity: AVAudioFrameCount(filled + joined)),
            let fill = filler.floatChannelData
        else { return }
        let last = length - 1
        let fading = Float(Self.fadedPackets * packetFrames)
        for channel in 0..<Int(outputFormat.channelCount) {
            for frame in 0..<(filled + joined) {
                let turn = (frame + 1) % (2 * last)
                let index = turn <= last ? last - turn : turn - last
                fill[channel][frame] = source[channel][index] * max(0, 1 - Float(frame) / fading)
            }
            for frame in 0..<joined {
                let weight = Float(frame + 1) / Float(joined + 1)
                destination[channel][frame] = fill[channel][filled + frame] * (1 - weight) + destination[channel][frame] * weight
            }
        }
        if filled > 0 {
            write(filler, frames: filled)
        }
    }

    private func write(_ buffer: AVAudioPCMBuffer, frames: Int) {
        if !ring.write(buffer, frames: frames) {
            ring.flushRequested.store(true, ordering: .relaxed)
        }
    }

    private func decode(_ data: Data) -> AVAudioPCMBuffer? {
        guard !data.isEmpty else { return nil }
        let input: AVAudioBuffer
        if inputFormat.commonFormat == .pcmFormatInt16 {
            let frames = AVAudioFrameCount(data.count / Int(inputFormat.streamDescription.pointee.mBytesPerFrame))
            guard let samples = AVAudioPCMBuffer(pcmFormat: inputFormat, frameCapacity: frames), let destination = samples.int16ChannelData
            else { return nil }
            data.withUnsafeBytes { raw in
                UnsafeMutableRawPointer(destination[0]).copyMemory(from: raw.baseAddress!, byteCount: data.count)
            }
            samples.frameLength = frames
            input = samples
        } else {
            let compressed = AVAudioCompressedBuffer(format: inputFormat, packetCapacity: 1, maximumPacketSize: data.count)
            data.withUnsafeBytes { raw in
                compressed.data.copyMemory(from: raw.baseAddress!, byteCount: data.count)
            }
            compressed.byteLength = UInt32(data.count)
            compressed.packetCount = 1
            compressed.packetDescriptions?.pointee = AudioStreamPacketDescription(
                mStartOffset: 0, mVariableFramesInPacket: 0, mDataByteSize: UInt32(data.count))
            input = compressed
        }
        guard let pcm = AVAudioPCMBuffer(pcmFormat: outputFormat, frameCapacity: AVAudioFrameCount(packetFrames)) else { return nil }
        var consumed = false
        var error: NSError?
        let status = converter.convert(to: pcm, error: &error) { _, outStatus in
            if consumed {
                outStatus.pointee = .noDataNow
                return nil
            }
            consumed = true
            outStatus.pointee = .haveData
            return input
        }
        guard status != .error, pcm.frameLength > 0 else { return nil }
        return pcm
    }
}

nonisolated final class AudioRing: @unchecked Sendable {
    let channels: Int
    private let capacity: Int
    private let mask: Int
    private let samples: UnsafeMutablePointer<Float>
    let written = ManagedAtomic<Int>(0)
    let read = ManagedAtomic<Int>(0)
    let target = ManagedAtomic<Int>(0)
    let flushRequested = ManagedAtomic<Bool>(false)

    private static let history = 4

    init(channels: Int, frames: Int) {
        self.channels = max(1, channels)
        var capacity = 1
        while capacity < frames {
            capacity <<= 1
        }
        self.capacity = capacity
        mask = capacity - 1
        samples = .allocate(capacity: self.channels * capacity)
        samples.initialize(repeating: 0, count: self.channels * capacity)
    }

    deinit {
        samples.deallocate()
    }

    func write(_ buffer: AVAudioPCMBuffer, frames: Int) -> Bool {
        guard frames > 0, let source = buffer.floatChannelData else { return true }
        let end = written.load(ordering: .relaxed)
        guard end + frames - read.load(ordering: .acquiring) <= capacity - Self.history else { return false }
        let sourceChannels = Int(buffer.format.channelCount)
        for channel in 0..<channels {
            let from = source[min(channel, sourceChannels - 1)]
            let base = samples + channel * capacity
            var done = 0
            var offset = end & mask
            while done < frames {
                let count = min(frames - done, capacity - offset)
                (base + offset).update(from: from + done, count: count)
                done += count
                offset = 0
            }
        }
        written.store(end + frames, ordering: .releasing)
        return true
    }

    @inline(__always)
    func sample(_ channel: Int, _ index: Int) -> Float {
        samples[channel &* capacity &+ (index & mask)]
    }
}

nonisolated final class AudioOutput: @unchecked Sendable {
    private let ring: AudioRing
    private let sampleRate: Double
    private let fadeFrames: Int
    private let skipFrames: Double
    private var position = 0
    private var phase = 0.0
    private var playing = false
    private var fadeIn = 0
    private var smoothedSlack = 0.0
    private var adjusting = false

    private static let fadeDuration = 0.005
    private static let lookahead = 3
    private static let slackSmoothing = 0.5
    private static let rateGain = 0.25
    private static let fasterLimit = 0.01
    private static let slowerLimit = 0.005
    private static let driftStart = 0.01
    private static let driftSettled = 0.002
    private static let skipThreshold = 0.08

    static func fadeFrames(sampleRate: Double) -> Int {
        max(1, Int((fadeDuration * sampleRate).rounded()))
    }

    init(ring: AudioRing, sampleRate: Double) {
        self.ring = ring
        self.sampleRate = sampleRate
        fadeFrames = Self.fadeFrames(sampleRate: sampleRate)
        skipFrames = Self.skipThreshold * sampleRate
    }

    func render(_ count: Int, into list: UnsafeMutableAudioBufferListPointer) -> Bool {
        if ring.flushRequested.exchange(false, ordering: .acquiringAndReleasing) {
            position = ring.written.load(ordering: .acquiring)
            phase = 0
            playing = false
            ring.read.store(position, ordering: .releasing)
        }
        let written = ring.written.load(ordering: .acquiring)
        let target = Double(ring.target.load(ordering: .relaxed))
        let reserve = Double(fadeFrames + Self.lookahead)
        var slack = Double(written - position) - phase - Double(count) - reserve
        if !playing {
            guard slack >= target else {
                silence(list, from: 0, to: count)
                return true
            }
            if slack - target > skipFrames {
                let skipped = Int(slack - target)
                position += skipped
                slack -= Double(skipped)
            }
            playing = true
            fadeIn = fadeFrames
            smoothedSlack = slack
            adjusting = false
        }
        smoothedSlack += (slack - smoothedSlack) * min(1, Double(count) / (sampleRate * Self.slackSmoothing))
        var skip = 0
        if (smoothedSlack - target) > skipFrames, slack - target > Double(fadeFrames) {
            skip = Int(slack - target)
            smoothedSlack -= Double(skip)
        }
        let error = (smoothedSlack - target) / sampleRate
        if adjusting {
            adjusting = abs(error) >= Self.driftSettled
        } else {
            adjusting = abs(error) > Self.driftStart
        }
        let rate = adjusting ? 1 + min(max(error * Self.rateGain, -Self.slowerLimit), Self.fasterLimit) : 1
        let playable = max(0, Int((Double(written - position - skip - Self.lookahead) - phase) / rate))
        let runningDry = playable < count + fadeFrames
        let frames = min(count, playable)
        let fadeOut = runningDry ? min(fadeFrames, frames) : 0
        let crossfade = skip > 0 ? min(fadeFrames, frames) : 0
        var index = position
        var fraction = phase
        if skip > 0, crossfade == 0 {
            index += skip
        }
        let channels = list.count
        for frame in 0..<frames {
            var gain: Float = 1
            if fadeIn > 0 {
                gain = 1 - Float(fadeIn) / Float(fadeFrames)
                fadeIn -= 1
            }
            if frame >= frames - fadeOut {
                gain *= Float(frames - frame) / Float(fadeOut + 1)
            }
            let weight = frame < crossfade ? Float(frame + 1) / Float(crossfade + 1) : 0
            for channel in 0..<channels {
                guard let output = list[channel].mData?.assumingMemoryBound(to: Float.self) else { continue }
                let source = min(channel, ring.channels - 1)
                var value = interpolate(source, index, fraction)
                if frame < crossfade {
                    value = value * (1 - weight) + interpolate(source, index + skip, fraction) * weight
                }
                output[frame] = value * gain
            }
            fraction += rate
            let whole = Int(fraction)
            index += whole
            fraction -= Double(whole)
            if frame + 1 == crossfade {
                index += skip
            }
        }
        silence(list, from: frames, to: count)
        position = index
        phase = fraction
        ring.read.store(position, ordering: .releasing)
        if runningDry {
            playing = false
        }
        return frames == 0
    }

    @inline(__always)
    private func interpolate(_ channel: Int, _ index: Int, _ phase: Double) -> Float {
        let b = ring.sample(channel, index)
        guard phase > 0 else { return b }
        let a = ring.sample(channel, index - 1)
        let c = ring.sample(channel, index + 1)
        let d = ring.sample(channel, index + 2)
        let t = Float(phase)
        return b + 0.5 * t * (c - a + t * (2 * a - 5 * b + 4 * c - d + t * (3 * (b - c) + d - a)))
    }

    private func silence(_ list: UnsafeMutableAudioBufferListPointer, from start: Int, to end: Int) {
        guard start < end else { return }
        for buffer in list {
            buffer.mData?.assumingMemoryBound(to: Float.self).advanced(by: start).update(repeating: 0, count: end - start)
        }
    }
}

nonisolated extension AVAudioEngine {
    // -[AVAudioEngine dealloc] stops the engine, then raises "required condition is false: false == isEngineRunning" from
    // AUGraphNodeIOV3's destructor if it still reports running, which terminates the process (AVFAudio, macOS 27.0.1 26A434,
    // UUID 72811002-C95F-3F3D-B4A6-5A2AA781EBB0, -[AVAudioEngine dealloc] 0x21baa0918..0x21baa0968,
    // AUGraphNodeIOV3::DeallocateInputBlock() 0x21b9eccfc..0x21b9eced0). On that build stop always clears the running state
    // (AVAudioEngineGraph::_Stop 0x21ba7c020..0x21ba7c09c), but the crash was observed on macOS 26.5.2 25F84 9 seconds after wake.
    func stopForRelease() {
        stop()
        guard isRunning else { return }
        DispatchQueue.global().asyncAfter(deadline: .now() + 1) { [self] in
            stopForRelease()
        }
    }
}

#if os(iOS)
nonisolated enum RemoteAudioSession {
    private static let recorders = LockedState(initialState: 0)

    static func activate() throws {
        try recorders.withLock { try configure(recording: $0 > 0) }
    }

    static func beginRecording() throws {
        try recorders.withLock { recorders in
            try configure(recording: true)
            recorders += 1
        }
    }

    static func endRecording() {
        recorders.withLock { recorders in
            recorders -= 1
            if recorders == 0 {
                try? configure(recording: false)
            }
        }
    }

    private static func configure(recording: Bool) throws {
        let session = AVAudioSession.sharedInstance()
        if recording {
            try session.setCategory(
                .playAndRecord, mode: .default, options: [.mixWithOthers, .defaultToSpeaker, .allowBluetoothHFP, .allowBluetoothA2DP])
        } else {
            try session.setCategory(.playback, mode: .default, options: [.mixWithOthers])
        }
        try session.setActive(true)
    }
}
#endif
