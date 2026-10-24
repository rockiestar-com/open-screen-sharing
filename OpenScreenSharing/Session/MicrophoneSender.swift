import AVFoundation
import Foundation

nonisolated final class MicrophoneSender: @unchecked Sendable {
    let format: AudioFormat
    private let queue = DispatchQueue(label: "MicrophoneSender")
    private let engine = AVAudioEngine()
    private let monoFormat: AVAudioFormat
    private let send: @Sendable (Data) -> Void
    private let failed: @Sendable (MicrophoneSender, any Error) -> Void
    private var running = false
    private var observers: [any NSObjectProtocol] = []

    static func requestPermission() async -> Bool {
        #if os(iOS)
        guard #available(iOS 17, *) else {
            return await withCheckedContinuation { continuation in
                AVAudioSession.sharedInstance().requestRecordPermission { @Sendable granted in
                    continuation.resume(returning: granted)
                }
            }
        }
        #endif
        return await AVAudioApplication.requestRecordPermission()
    }

    init(format: AudioFormat, send: @escaping @Sendable (Data) -> Void, failed: @escaping @Sendable (MicrophoneSender, any Error) -> Void) throws {
        guard let monoFormat = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: format.sampleRate, channels: 1, interleaved: false)
        else { throw MessageError("AVAudioFormat rejected \(Int(format.sampleRate)) Hz mono") }
        self.format = format
        self.monoFormat = monoFormat
        self.send = send
        self.failed = failed
        let center = NotificationCenter.default
        observers.append(
            center.addObserver(forName: .AVAudioEngineConfigurationChange, object: engine, queue: nil) { [weak self] _ in
                self?.restart()
            })
        #if os(iOS)
        observers.append(
            center.addObserver(forName: AVAudioSession.interruptionNotification, object: AVAudioSession.sharedInstance(), queue: nil) {
                [weak self] notification in
                guard (notification.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt).flatMap(AVAudioSession.InterruptionType.init) == .ended
                else { return }
                self?.restart()
            })
        #endif
    }

    deinit {
        for observer in observers {
            NotificationCenter.default.removeObserver(observer)
        }
        engine.stopForRelease()
    }

    func start() {
        queue.async { [self] in
            do {
                #if os(iOS)
                try RemoteAudioSession.beginRecording()
                #endif
                do {
                    try startEngine()
                } catch {
                    #if os(iOS)
                    RemoteAudioSession.endRecording()
                    #endif
                    throw error
                }
                running = true
            } catch {
                failed(self, error)
            }
        }
    }

    func stop() {
        queue.async { [self] in
            guard running else { return }
            running = false
            engine.inputNode.removeTap(onBus: 0)
            engine.stop()
            #if os(iOS)
            RemoteAudioSession.endRecording()
            #endif
        }
    }

    private func startEngine() throws {
        let input = engine.inputNode
        let inputFormat = input.outputFormat(forBus: 0)
        guard inputFormat.sampleRate > 0, inputFormat.channelCount > 0, let resampler = AVAudioConverter(from: inputFormat, to: monoFormat) else {
            throw MessageError("AVAudioConverter can't convert the microphone's \(inputFormat) to \(monoFormat)")
        }
        resampler.downmix = true
        input.installTap(onBus: 0, bufferSize: 1024, format: inputFormat) { [weak self] buffer, _ in
            self?.process(buffer, resampler: resampler)
        }
        engine.prepare()
        try engine.start()
    }

    private func restart() {
        queue.async { [self] in
            guard running else { return }
            // AVAudioEngineConfigurationChange can arrive while the engine still reports running, although its documentation
            // says the engine stops itself first (iOS 26.4.2, https://developer.apple.com/forums/thread/825253).
            engine.stop()
            engine.inputNode.removeTap(onBus: 0)
            do {
                #if os(iOS)
                try RemoteAudioSession.activate()
                #endif
                try startEngine()
            } catch {
                failed(self, error)
            }
        }
    }

    private func process(_ buffer: AVAudioPCMBuffer, resampler: AVAudioConverter) {
        let capacity = AVAudioFrameCount(Double(buffer.frameLength) * format.sampleRate / buffer.format.sampleRate) + 32
        guard let mono = AVAudioPCMBuffer(pcmFormat: monoFormat, frameCapacity: capacity) else { return }
        var consumed = false
        var error: NSError?
        resampler.convert(to: mono, error: &error) { _, status in
            if consumed {
                status.pointee = .noDataNow
                return nil
            }
            consumed = true
            status.pointee = .haveData
            return buffer
        }
        guard error == nil, let samples = mono.floatChannelData?[0], mono.frameLength > 0 else { return }
        let frames = Int(mono.frameLength)
        let channels = Int(format.channels)
        var pcm = Data(count: frames * channels * MemoryLayout<Int16>.size)
        pcm.withUnsafeMutableBytes { raw in
            let output = raw.bindMemory(to: Int16.self)
            for frame in 0..<frames {
                let sample = Int16(max(-1, min(1, samples[frame])) * Float(Int16.max)).littleEndian
                for channel in 0..<channels {
                    output[frame * channels + channel] = sample
                }
            }
        }
        send(pcm)
    }
}
