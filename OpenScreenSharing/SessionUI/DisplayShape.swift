#if os(iOS)
import UIKit

struct DisplayShape {
    // Portrait shapes keyed by the model identifiers of the CoreSimulator device type profiles. width is the profile's
    // canvasWidth, or width where it has none, / scale. cutout is UIKitCore __UIScreenNativeDisplayExclusionRectForType
    // (iOS 27.2 24B5110a, UUID 56EB6C8F-D489-378F-AABA-81975A6D213E, 0x184913404) and, for iPhone19,x, SpringBoard
    // sb_displayShapeForScreenType: (iOS 27.2 24B5110a, UUID DC16B497-8B4A-323B-BA45-84E3A4DC5307, 0x226ff8580);
    // iPhone10,x and iPhone11,x share the ArtworkDeviceSubType of the iPhone12,x with their cutout. cornerRadius is the
    // profile's DeviceCornerRadius MobileGestalt answer, which UIKitCore -[UIScreen _displayCornerRadius] returns (block
    // 0x18485c2d8), for the profiles with a minRuntimeVersion below 26, except iPad Air (3rd generation), which answers
    // 18 for its square display. Display Zoom scales the shape by the zoomed width, as the radii of the zoomed screen
    // types in UIKitCore __UIScreenCornerRadiusForType (0x1847c3b74) are.
    private static let portraitShapes: [String: (width: CGFloat, cutout: CGRect?, cornerRadius: CGFloat?)] = [
        "iPhone10,3": (375, CGRect(x: 83, y: 0, width: 209, height: 30), 39),
        "iPhone10,6": (375, CGRect(x: 83, y: 0, width: 209, height: 30), 39),
        "iPhone11,2": (375, CGRect(x: 83, y: 0, width: 209, height: 30), 39),
        "iPhone11,4": (414, CGRect(x: 307.0 / 3, y: 0, width: 628.0 / 3, height: 30), 39),
        "iPhone11,6": (414, CGRect(x: 307.0 / 3, y: 0, width: 628.0 / 3, height: 30), 39),
        "iPhone11,8": (414, CGRect(x: 93.5, y: 0, width: 227, height: 32), 41.5),
        "iPhone12,1": (414, CGRect(x: 93.5, y: 0, width: 227, height: 32), 41.5),
        "iPhone12,3": (375, CGRect(x: 83, y: 0, width: 209, height: 30), 39),
        "iPhone12,5": (414, CGRect(x: 307.0 / 3, y: 0, width: 628.0 / 3, height: 30), 39),
        "iPhone13,1": (375, CGRect(x: 74.5, y: 0, width: 226, height: 103.0 / 3), 44),
        "iPhone13,2": (390, CGRect(x: 90, y: 0, width: 210, height: 32), 47),
        "iPhone13,3": (390, CGRect(x: 90, y: 0, width: 210, height: 32), 47),
        "iPhone13,4": (428, CGRect(x: 328.0 / 3, y: 0, width: 628.0 / 3, height: 32), 53),
        "iPhone14,2": (390, CGRect(x: 343.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), 142.0 / 3),
        "iPhone14,3": (428, CGRect(x: 400.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), 160.0 / 3),
        "iPhone14,4": (375, CGRect(x: 100.5, y: 0, width: 174, height: 112.0 / 3), 44),
        "iPhone14,5": (390, CGRect(x: 343.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), 142.0 / 3),
        "iPhone14,7": (390, CGRect(x: 343.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), 142.0 / 3),
        "iPhone14,8": (428, CGRect(x: 400.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), 160.0 / 3),
        "iPhone15,2": (393, CGRect(x: 134, y: 34.0 / 3, width: 125, height: 110.0 / 3), 55),
        "iPhone15,3": (430, CGRect(x: 457.0 / 3, y: 34.0 / 3, width: 376.0 / 3, height: 110.0 / 3), 55),
        "iPhone15,4": (393, CGRect(x: 134, y: 34.0 / 3, width: 125, height: 110.0 / 3), 55),
        "iPhone15,5": (430, CGRect(x: 457.0 / 3, y: 34.0 / 3, width: 376.0 / 3, height: 110.0 / 3), 55),
        "iPhone16,1": (393, CGRect(x: 134, y: 34.0 / 3, width: 125, height: 110.0 / 3), 55),
        "iPhone16,2": (430, CGRect(x: 457.0 / 3, y: 34.0 / 3, width: 376.0 / 3, height: 110.0 / 3), 55),
        "iPhone17,1": (402, CGRect(x: 415.0 / 3, y: 14, width: 125, height: 110.0 / 3), 62),
        "iPhone17,2": (440, CGRect(x: 472.0 / 3, y: 14, width: 376.0 / 3, height: 110.0 / 3), 62),
        "iPhone17,3": (393, CGRect(x: 134, y: 34.0 / 3, width: 125, height: 110.0 / 3), 55),
        "iPhone17,4": (430, CGRect(x: 457.0 / 3, y: 34.0 / 3, width: 376.0 / 3, height: 110.0 / 3), 55),
        "iPhone17,5": (390, CGRect(x: 343.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), 142.0 / 3),
        "iPhone18,1": (402, CGRect(x: 415.0 / 3, y: 14, width: 125, height: 110.0 / 3), nil),
        "iPhone18,2": (440, CGRect(x: 472.0 / 3, y: 14, width: 376.0 / 3, height: 110.0 / 3), nil),
        "iPhone18,3": (402, CGRect(x: 415.0 / 3, y: 14, width: 125, height: 110.0 / 3), nil),
        "iPhone18,4": (420, CGRect(x: 442.0 / 3, y: 20, width: 376.0 / 3, height: 110.0 / 3), nil),
        "iPhone18,5": (390, CGRect(x: 343.0 / 3, y: 0, width: 484.0 / 3, height: 101.0 / 3), nil),
        "iPhone19,2": (402, CGRect(x: 461.0 / 3, y: 14, width: 284.0 / 3, height: 110.0 / 3), nil),
        "iPhone19,3": (440, CGRect(x: 518.0 / 3, y: 14, width: 284.0 / 3, height: 110.0 / 3), nil),
        "iPhone19,7": (440, CGRect(x: 518.0 / 3, y: 14, width: 284.0 / 3, height: 110.0 / 3), nil),
        "iPad8,1": (834, nil, 18),
        "iPad8,2": (834, nil, 18),
        "iPad8,3": (834, nil, 18),
        "iPad8,4": (834, nil, 18),
        "iPad8,5": (1024, nil, 18),
        "iPad8,6": (1024, nil, 18),
        "iPad8,7": (1024, nil, 18),
        "iPad8,8": (1024, nil, 18),
        "iPad8,9": (834, nil, 18),
        "iPad8,10": (834, nil, 18),
        "iPad8,11": (1024, nil, 18),
        "iPad8,12": (1024, nil, 18),
        "iPad13,1": (820, nil, 18),
        "iPad13,2": (820, nil, 18),
        "iPad13,4": (834, nil, 18),
        "iPad13,5": (834, nil, 18),
        "iPad13,6": (834, nil, 18),
        "iPad13,7": (834, nil, 18),
        "iPad13,8": (1024, nil, 18),
        "iPad13,9": (1024, nil, 18),
        "iPad13,10": (1024, nil, 18),
        "iPad13,11": (1024, nil, 18),
        "iPad13,16": (820, nil, 18),
        "iPad13,17": (820, nil, 18),
        "iPad13,18": (820, nil, 25),
        "iPad13,19": (820, nil, 25),
        "iPad14,1": (744, nil, 21.5),
        "iPad14,2": (744, nil, 21.5),
        "iPad14,3": (834, nil, 18),
        "iPad14,4": (834, nil, 18),
        "iPad14,5": (1024, nil, 18),
        "iPad14,6": (1024, nil, 18),
        "iPad14,8": (820, nil, 18),
        "iPad14,9": (820, nil, 18),
        "iPad14,10": (1024, nil, 18),
        "iPad14,11": (1024, nil, 18),
        "iPad15,3": (820, nil, 18),
        "iPad15,4": (820, nil, 18),
        "iPad15,5": (1024, nil, 18),
        "iPad15,6": (1024, nil, 18),
        "iPad15,7": (820, nil, 25),
        "iPad15,8": (820, nil, 25),
        "iPad16,1": (744, nil, 21.5),
        "iPad16,2": (744, nil, 21.5),
        "iPad16,3": (834, nil, 30),
        "iPad16,4": (834, nil, 30),
        "iPad16,5": (1032, nil, 30),
        "iPad16,6": (1032, nil, 30),
    ]

    static var describesDevice: Bool {
        portraitShapes[DeviceModel.identifier] != nil
    }

    let cutout: CGRect?
    let cornerRadius: CGFloat?

    init?(screen: UIScreen, orientation: UIInterfaceOrientation) {
        guard screen == UIScreen.main, let shape = Self.portraitShapes[DeviceModel.identifier] else { return nil }
        let size = screen.bounds.size
        let width = min(size.width, size.height)
        let height = max(size.width, size.height)
        let zoom = width / shape.width
        cutout = shape.cutout.map { cutout in
            let rect = CGRect(x: cutout.minX * zoom, y: cutout.minY * zoom, width: cutout.width * zoom, height: cutout.height * zoom)
            return switch orientation {
            case .portraitUpsideDown: CGRect(x: width - rect.maxX, y: height - rect.maxY, width: rect.width, height: rect.height)
            case .landscapeLeft: CGRect(x: height - rect.maxY, y: rect.minX, width: rect.height, height: rect.width)
            case .landscapeRight: CGRect(x: rect.minY, y: width - rect.maxX, width: rect.height, height: rect.width)
            default: rect
            }
        }
        cornerRadius = shape.cornerRadius.map { $0 * zoom }
    }
}
#endif
