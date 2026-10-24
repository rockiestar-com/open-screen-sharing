package rfb

import (
	"fmt"
)

type Encoding int32

const (
	EncodingRaw                 Encoding = 0
	EncodingCopyRect            Encoding = 1
	EncodingHextile             Encoding = 5
	EncodingZlib                Encoding = 6
	EncodingZRLE                Encoding = 16
	EncodingAppleHalftone       Encoding = 1000
	EncodingAppleGray           Encoding = 1001
	EncodingAppleThousands      Encoding = 1002
	EncodingMediaStream         Encoding = 1010
	EncodingMVS                 Encoding = 1011
	EncodingCursorPosition      Encoding = 1100
	EncodingDisplayInfo         Encoding = 1101
	EncodingUserInfo            Encoding = 1102
	EncodingRekey               Encoding = 1103
	EncodingCursorImage         Encoding = 1104
	EncodingDisplayLayout       Encoding = 1105
	EncodingVendorKeysyms       Encoding = 1107
	EncodingKeyboardSource      Encoding = 1109
	EncodingDeviceInfo          Encoding = 1110
	EncodingRichCursor          Encoding = -239
	EncodingPointerPosition     Encoding = -232
	EncodingLastRect            Encoding = -224
	EncodingDesktopSize         Encoding = -223
	EncodingDesktopName         Encoding = -307
	EncodingExtendedDesktopSize Encoding = -308
)

var encodingNames = map[Encoding]string{
	EncodingRaw:                 "raw",
	EncodingCopyRect:            "copyrect",
	EncodingHextile:             "hextile",
	EncodingZlib:                "zlib",
	EncodingZRLE:                "zrle",
	EncodingAppleHalftone:       "apple-halftone",
	EncodingAppleGray:           "apple-gray",
	EncodingAppleThousands:      "apple-thousands",
	EncodingMediaStream:         "media-stream",
	EncodingMVS:                 "mvs",
	EncodingCursorPosition:      "cursor-position",
	EncodingDisplayInfo:         "display-info",
	EncodingUserInfo:            "user-info",
	EncodingRekey:               "rekey",
	EncodingCursorImage:         "cursor-image",
	EncodingDisplayLayout:       "display-layout",
	EncodingVendorKeysyms:       "vendor-keysyms",
	EncodingKeyboardSource:      "keyboard-source",
	EncodingDeviceInfo:          "device-info",
	EncodingRichCursor:          "rich-cursor",
	EncodingPointerPosition:     "pointer-position",
	EncodingLastRect:            "last-rect",
	EncodingDesktopSize:         "desktop-size",
	EncodingDesktopName:         "desktop-name",
	EncodingExtendedDesktopSize: "extended-desktop-size",
}

func (e Encoding) String() string {
	name, found := encodingNames[e]
	if found {
		return name
	}
	return fmt.Sprint("encoding(", int32(e), ")")
}

func (e Encoding) carriesPixels() bool {
	switch e {
	case EncodingRaw, EncodingCopyRect, EncodingHextile, EncodingZlib, EncodingZRLE,
		EncodingAppleHalftone, EncodingAppleGray, EncodingAppleThousands, EncodingMVS:
		return true
	default:
		return false
	}
}

var appleMetadataEncodings = []Encoding{
	EncodingCursorImage,
	EncodingCursorPosition,
	EncodingDesktopSize,
	EncodingDisplayInfo,
	EncodingDisplayLayout,
	EncodingVendorKeysyms,
	EncodingKeyboardSource,
	EncodingDeviceInfo,
}

var standardMetadataEncodings = []Encoding{
	EncodingRichCursor,
	EncodingPointerPosition,
	EncodingDesktopSize,
	EncodingExtendedDesktopSize,
	EncodingDesktopName,
	EncodingLastRect,
}

func applePictureEncodings(quality Quality) []Encoding {
	var picture []Encoding
	switch quality {
	case QualityAdaptive:
		picture = []Encoding{EncodingMVS, EncodingAppleThousands, EncodingZlib, EncodingZRLE}
	case QualityFull:
		picture = []Encoding{EncodingZlib, EncodingZRLE}
	case QualityHigh:
		picture = []Encoding{EncodingAppleThousands, EncodingZlib, EncodingZRLE}
	case QualityMedium:
		picture = []Encoding{EncodingAppleGray, EncodingZlib, EncodingZRLE}
	case QualityLow:
		picture = []Encoding{EncodingAppleHalftone, EncodingZlib, EncodingZRLE}
	}
	return picture
}

var standardPictureEncodings = []Encoding{
	EncodingZRLE,
	EncodingHextile,
	EncodingZlib,
	EncodingCopyRect,
	EncodingRaw,
}
