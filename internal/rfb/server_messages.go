package rfb

import (
	"encoding/binary"
	"math"
)

// ScreenSharing reads the virtual-display limit from ServerInit flag bits
// 5..7 when bit 0x10 is set; otherwise it uses two
// (ScreenSharing 776.27, macOS 27.0.1 26A434,
// UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
// _RFBAuthenticateCore 0x1cc141280..0x1cc14129c,
// _RFBMaxVirtualDisplays 0x1cc1498d8..0x1cc1498e8).
const serverFlags = ServerFlagExtended | ServerFlagControlPrivilege | 1<<5

var serverCommands = func() [16]byte {
	var bitmap [16]byte
	for _, command := range []uint8{
		0x00, 0x02, 0x03, 0x04, 0x05, 0x06, 0x09, 0x0a, 0x0b, 0x10, 0x12, 0x15, 0x17, 0x1c, 0x1d, 0x1f, 0x21,
	} {
		bitmap[command/8] |= 0x80 >> (command % 8)
	}
	return bitmap
}()

func serverInitMessage(width, height int, name string) []byte {
	message := binary.BigEndian.AppendUint16(nil, uint16(width))
	message = binary.BigEndian.AppendUint16(message, uint16(height))
	format := make([]byte, 16)
	pixelFormatBGRA.encode(format)
	message = append(message, format...)
	extended := []byte{0, 0}
	extended = binary.BigEndian.AppendUint32(extended, serverFlags)
	extended = append(extended, serverCommands[:]...)
	extended = append(extended, name...)
	message = binary.BigEndian.AppendUint32(message, uint32(len(extended)))
	return append(message, extended...)
}

type serverRectangle struct {
	header  []byte
	payload []byte
}

func newRectangle(x, y, width, height int, encoding Encoding, payload []byte) serverRectangle {
	header := binary.BigEndian.AppendUint16(nil, uint16(x))
	header = binary.BigEndian.AppendUint16(header, uint16(y))
	header = binary.BigEndian.AppendUint16(header, uint16(width))
	header = binary.BigEndian.AppendUint16(header, uint16(height))
	header = binary.BigEndian.AppendUint32(header, uint32(encoding))
	return serverRectangle{header: header, payload: payload}
}

func framebufferUpdate(rectangles ...serverRectangle) []byte {
	size := 4
	for _, rectangle := range rectangles {
		size += len(rectangle.header) + len(rectangle.payload)
	}
	message := make([]byte, 4, size)
	binary.BigEndian.PutUint16(message[2:], uint16(len(rectangles)))
	for _, rectangle := range rectangles {
		message = append(message, rectangle.header...)
		message = append(message, rectangle.payload...)
	}
	return message
}

func mediaStreamUpdate(payload []byte) []byte {
	return framebufferUpdate(newRectangle(0, 0, 0, 0, EncodingMediaStream, payload))
}

// ScreenSharing passes the desktop dimensions at layout-header offsets 2/4
// to setFrame:, and framebuffer dimensions at 6/8 to setScaledFrame:.
// Bit 0 of the u32 at header offset 14 sets isUsingVirtualDisplay; Dynamic
// Resolution also requires an AVC media stream and exactly one screen
// (ScreenSharing 776.27, macOS 27.0.1 26A434,
// UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
// _HandleFramebufferUpdate 0x1cc151a30..0x1cc151a68,
// -[SSSession handleDisplayInfo2:] 0x1cc0df7ec..0x1cc0df828,
// 0x1cc0e0aac..0x1cc0e0acc,
// -[SSSession handleDisplayInfo2b:screenFlags:screenConfiguration:newSelectedScreen:displayCountChanged:]
// 0x1cc0e1734..0x1cc0e1800,
// -[SSSessionView dynamicResolutionModeAvailable] 0x1cc0acacc..0x1cc0acb20).
func displayLayoutRectangle(screen ServerScreen, scale float64) serverRectangle {
	if !(scale > 0 && scale <= 1) {
		scale = 1
	}
	density := screen.Density()
	pointWidth := int(math.Round(float64(screen.Width) / density))
	pointHeight := int(math.Round(float64(screen.Height) / density))
	framebuffer := scaledFramebuffer(screen, scale)
	payload := make([]byte, 2, 2+layoutHeaderSize+layoutRecordSize)
	payload = binary.BigEndian.AppendUint16(payload, 5)
	payload = binary.BigEndian.AppendUint16(payload, uint16(pointWidth))
	payload = binary.BigEndian.AppendUint16(payload, uint16(pointHeight))
	payload = binary.BigEndian.AppendUint16(payload, uint16(framebuffer[0]))
	payload = binary.BigEndian.AppendUint16(payload, uint16(framebuffer[1]))
	payload = binary.BigEndian.AppendUint32(payload, AllDisplays)
	payload = binary.BigEndian.AppendUint32(payload, 4)
	payload = binary.BigEndian.AppendUint16(payload, 1)
	payload = binary.BigEndian.AppendUint64(payload, math.Float64bits(density))
	payload = binary.BigEndian.AppendUint64(payload, math.Float64bits(scale))
	payload = binary.BigEndian.AppendUint32(payload, 1)
	edges := func(width, height int) {
		payload = binary.BigEndian.AppendUint16(payload, 0)
		payload = binary.BigEndian.AppendUint16(payload, 0)
		payload = binary.BigEndian.AppendUint16(payload, uint16(height))
		payload = binary.BigEndian.AppendUint16(payload, uint16(width))
	}
	edges(pointWidth, pointHeight)
	edges(framebuffer[0], framebuffer[1])
	payload = binary.BigEndian.AppendUint32(payload, DisplayFlagMain)
	payload = append(payload, 0x20, 0x20, 0x00, 0x01, 0x00, 0xff, 0x00, 0xff, 0x00, 0xff, 0x10, 0x08, 0, 0, 0, 0)
	binary.BigEndian.PutUint16(payload, uint16(len(payload)-2))
	return newRectangle(0, 0, 0, 0, EncodingDisplayLayout, payload)
}

func scaledFramebuffer(screen ServerScreen, scale float64) [2]int {
	if !(scale > 0 && scale < 1) {
		return [2]int{screen.Width, screen.Height}
	}
	return [2]int{int(math.Round(float64(screen.Width) * scale)), int(math.Round(float64(screen.Height) * scale))}
}

// ScreenSharing treats bit 0 of the device-info flags, the u32 after the
// version, as support for dynamic drag (ScreenSharing 776.27,
// macOS 27.0.1 26A434, UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
// _HandleFramebufferUpdate 0x1cc1517fc..0x1cc151814,
// -[SSSession handleDeviceInfoEncoding:] 0x1cc0e9e50..0x1cc0e9e60).
func deviceInfoRectangle(model string) serverRectangle {
	fields := []string{model, "unknown", "unknown"}
	body := binary.BigEndian.AppendUint16(nil, 2)
	body = binary.BigEndian.AppendUint32(body, 0)
	body = binary.BigEndian.AppendUint32(body, 0)
	for _, field := range fields {
		body = binary.BigEndian.AppendUint16(body, uint16(len(field)+1))
	}
	for _, field := range fields {
		body = append(body, field...)
		body = append(body, 0)
	}
	body = binary.BigEndian.AppendUint32(body, 0)
	payload := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	return newRectangle(0, 0, 0, 0, EncodingDeviceInfo, append(payload, body...))
}

// ScreenSharing reads the vendor-keysym count at offset 2 and the u32
// keysyms at offset 4 (ScreenSharing 776.27, macOS 27.0.1 26A434,
// UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
// _HandleFramebufferUpdate 0x1cc1519bc..0x1cc1519e8,
// -[SSSession handleVendorKeysymEncoding:] 0x1cc0e3e10..0x1cc0e3e1c).
func vendorKeysymsRectangle(commands []SystemCommand) serverRectangle {
	body := binary.BigEndian.AppendUint16(nil, 1)
	body = binary.BigEndian.AppendUint16(body, uint16(len(commands)))
	for _, command := range commands {
		body = binary.BigEndian.AppendUint32(body, uint32(command))
	}
	payload := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	return newRectangle(0, 0, 0, 0, EncodingVendorKeysyms, append(payload, body...))
}

func statusMessage(status Status) []byte {
	message := []byte{0x14, 0, 0, 4, 0, 1}
	return binary.BigEndian.AppendUint16(message, uint16(status))
}
