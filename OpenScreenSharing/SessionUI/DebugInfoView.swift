import Perception
import SwiftUI
#if os(iOS)
import UIKit
#endif

struct DebugInfoView: View {
    let controller: SessionController

    @Environment(\.horizontalSizeClass) private var horizontalSizeClass

    private static let columns = 27
    private static let dropWarning = 1.0
    private static let responseWarning = 100.0
    private static let responseSevere = 200.0
    private static let latencyWarning = 100.0
    private static let latencySevere = 200.0
    private static let remoteWarning = 50.0
    private static let remoteSevere = 100.0
    private static let networkWarning = 25.0
    private static let networkSevere = 50.0
    private static let roundTripWarning = 50.0
    private static let roundTripSevere = 100.0
    private static let deviceWarning = 2000.0 / 60
    private static let deviceSevere = DebugInfoMonitor.stutter * 1000
    private static let lossWarning = 0.01
    private static let lossSevere = 0.05
    private static let stutterSevere = 10.0

    private static let deviceName: String = {
        #if os(iOS)
        switch UIDevice.current.userInterfaceIdiom {
        case .pad: "This iPad"
        case .phone: "This iPhone"
        default: "This Device"
        }
        #else
        "This Mac"
        #endif
    }()

    private var fontSize: CGFloat { horizontalSizeClass == .compact ? 11 : 12 }

    private var width: CGFloat {
        #if os(iOS)
        let font = UIFont.monospacedSystemFont(ofSize: fontSize, weight: .regular)
        #else
        let font = NSFont.monospacedSystemFont(ofSize: fontSize, weight: .regular)
        #endif
        return ("0" as NSString).size(withAttributes: [.font: font]).width * CGFloat(Self.columns)
    }

    var body: some View {
        WithPerceptionTracking {
            let monitor = controller.debugInfo
            let figures = monitor.figures
            let response = monitor.response
            let network = figures?.networkMilliseconds
            let remote = figures?.remoteMilliseconds
            let device = figures?.deviceMilliseconds
            let latency = network.flatMap { network in remote.flatMap { remote in device.map { remote + network + $0 } } }
            let roundTrip = response.map { $0.network * 1000 } ?? figures?.roundTripMilliseconds
            let responseDevice = response.map { $0.device * 1000 } ?? device
            let stutters = monitor.stutters
            let dialect = controller.info.dialect
            let isVideo = controller.isVideo
            let breaksDown = controller.usesHighPerformance
            VStack(spacing: 0) {
                pair {
                    Text(verbatim: dialect == .rdp ? "RDP" : isVideo ? "High Performance" : dialect == .apple ? "Standard" : "VNC")
                        .foregroundStyle(dialect != .apple ? Color.white : isVideo ? .green : .orange)
                } trailing: {
                    Text(verbatim: (isVideo ? controller.info.videoCodec : figures?.encoding) ?? "—")
                }
                pair {
                    Text(verbatim: imageSize)
                } trailing: {
                    Text(verbatim: figures?.shownPerSecond.map { String(format: "%.0f fps", $0) } ?? "—")
                        .foregroundStyle(dropsFrames(figures) ? Color.orange : .white)
                }
                VStack(spacing: 0) {
                    if breaksDown {
                        // Frame latency: remote compose -> network -> local presentation.
                        row(
                            "Latency", latency.map(Self.milliseconds),
                            color: Self.color(latency, warning: Self.latencyWarning, severe: Self.latencySevere))
                        row(
                            "  " + remoteName, remote.map(Self.milliseconds),
                            color: Self.color(remote, warning: Self.remoteWarning, severe: Self.remoteSevere))
                        row(
                            "  Network", network.map(Self.milliseconds),
                            color: Self.color(network, warning: Self.networkWarning, severe: Self.networkSevere))
                        row(
                            "  " + Self.deviceName, device.map(Self.milliseconds),
                            color: Self.color(device, warning: Self.deviceWarning, severe: Self.deviceSevere))
                        row(
                            "Response", response.map { Self.milliseconds($0.total * 1000) },
                            color: Self.color(response.map { $0.total * 1000 }, warning: Self.responseWarning, severe: Self.responseSevere))
                    } else {
                        // Input response: input sent -> round trip -> remote -> local presentation.
                        row(
                            "Response", response.map { Self.milliseconds($0.total * 1000) },
                            color: Self.color(response.map { $0.total * 1000 }, warning: Self.responseWarning, severe: Self.responseSevere))
                        row(
                            "  Network", roundTrip.map(Self.milliseconds),
                            color: Self.color(roundTrip, warning: Self.roundTripWarning, severe: Self.roundTripSevere))
                        row(
                            "  " + remoteName, response.map { Self.milliseconds($0.remote * 1000) },
                            color: Self.color(response.map { $0.remote * 1000 }, warning: Self.remoteWarning, severe: Self.remoteSevere))
                        row(
                            "  " + Self.deviceName, responseDevice.map(Self.milliseconds),
                            color: Self.color(responseDevice, warning: Self.deviceWarning, severe: Self.deviceSevere))
                    }
                    row("Bitrate", figures?.bitsPerSecond.map(Self.bitrate))
                    if figures?.lossy == true {
                        row(
                            "Packet Loss", figures?.loss.map { String(format: "%.1f%%", $0 * 100) },
                            color: Self.color(figures?.loss, warning: Self.lossWarning, severe: Self.lossSevere))
                    }
                    row("Stutters", "\(stutters)", color: Self.color(Double(stutters), warning: 1, severe: Self.stutterSevere))
                }
                .padding(.top, 4)
                StutterTimeline(monitor: monitor)
                    .frame(height: fontSize * 3)
                    .padding(.top, 2)
                    .accessibilityHidden(true)
            }
            .font(.system(size: fontSize, weight: .regular, design: .monospaced))
            .monospacedDigit()
            .lineLimit(1)
            .foregroundStyle(.white)
            .frame(width: width)
            .padding(.horizontal, 8)
            .padding(.vertical, 6)
            .background(.black.opacity(0.8), in: RoundedRectangle(cornerRadius: 8))
            .environment(\.colorScheme, .dark)
            .accessibilityElement(children: .combine)
            .task {
                let monitor = controller.debugInfo
                monitor.start()
                while !Task.isCancelled {
                    controller.sampleDebugInfo()
                    try? await Task.sleep(seconds: 1)
                }
                monitor.stop()
            }
        }
    }

    private var remoteName: String {
        switch controller.platform {
        case .mac: "Mac"
        case .pc: "PC"
        case .iOS: "Remote"
        }
    }

    private var imageSize: String {
        let size = controller.imageSize
        guard size.width > 0, size.height > 0 else { return "—" }
        return "\(Int(size.width))x\(Int(size.height))"
    }

    private func dropsFrames(_ figures: DebugInfoMonitor.Figures?) -> Bool {
        guard controller.isVideo, let ready = figures?.readyPerSecond, let shown = figures?.shownPerSecond else { return false }
        return ready - shown > Self.dropWarning
    }

    private func pair(@ViewBuilder leading: () -> some View, @ViewBuilder trailing: () -> some View) -> some View {
        HStack(spacing: 0) {
            leading()
            Spacer(minLength: 8)
            HStack(spacing: 0) {
                trailing()
            }
            .layoutPriority(1)
        }
    }

    private func row(_ label: String, _ value: String?, color: Color = .white) -> some View {
        pair {
            Text(verbatim: label)
        } trailing: {
            Text(verbatim: value ?? "—")
                .foregroundStyle(color)
        }
    }

    private static func color(_ value: Double?, warning: Double, severe: Double) -> Color {
        guard let value else { return .white }
        return value >= severe ? .red : value >= warning ? .orange : .white
    }

    private static func bitrate(_ bitsPerSecond: Double) -> String {
        bitsPerSecond >= 1_000_000 ? String(format: "%.1fMbps", bitsPerSecond / 1_000_000) : String(format: "%.0fkbps", bitsPerSecond / 1000)
    }

    private static func milliseconds(_ value: Double) -> String {
        (0..<1).contains(value) ? "<1ms" : String(format: "%.0fms", value)
    }
}

private struct StutterTimeline: View {
    let monitor: DebugInfoMonitor

    @Environment(\.displayScale) private var displayScale

    private static let top = DebugInfoMonitor.stutter * 4
    private static let minutes = DebugInfoMonitor.spans * DebugInfoMonitor.spanSeconds / 60

    var body: some View {
        WithPerceptionTracking {
            let spans = monitor.timeline
            Canvas { context, size in
                let pixel = 1 / displayScale
                var grid = Path()
                for minute in 1..<Self.minutes {
                    let x = size.width * CGFloat(minute) / CGFloat(Self.minutes)
                    grid.move(to: CGPoint(x: x, y: 0))
                    grid.addLine(to: CGPoint(x: x, y: size.height))
                }
                let stutterLine = (size.height - 1) * (1 - CGFloat(DebugInfoMonitor.stutter / Self.top))
                grid.move(to: CGPoint(x: 0, y: stutterLine))
                grid.addLine(to: CGPoint(x: size.width, y: stutterLine))
                context.stroke(grid, with: .color(.white.opacity(0.3)), lineWidth: pixel)
                var axes = Path()
                axes.move(to: CGPoint(x: pixel / 2, y: 0))
                axes.addLine(to: CGPoint(x: pixel / 2, y: size.height - pixel / 2))
                axes.addLine(to: CGPoint(x: size.width - pixel / 2, y: size.height - pixel / 2))
                axes.addLine(to: CGPoint(x: size.width - pixel / 2, y: 0))
                context.stroke(axes, with: .color(.white), lineWidth: pixel)

                let width = size.width / CGFloat(DebugInfoMonitor.spans)
                var smooth = Path()
                var stuttered = Path()
                for (index, span) in spans.enumerated() {
                    guard let lateness = span.stutters > 0 ? max(span.lateness ?? 0, DebugInfoMonitor.stutter) : span.lateness else { continue }
                    let height = max((size.height - 1) * CGFloat(min(lateness / Self.top, 1)), pixel)
                    let x = size.width - CGFloat(spans.count - index) * width
                    let bar = CGRect(x: x + pixel, y: size.height - pixel - height, width: max(width - pixel * 2, pixel), height: height)
                    if span.stutters > 0 {
                        stuttered.addRect(bar)
                    } else {
                        smooth.addRect(bar)
                    }
                }
                context.fill(smooth, with: .color(.blue))
                context.fill(stuttered, with: .color(.red))
            }
        }
    }
}
