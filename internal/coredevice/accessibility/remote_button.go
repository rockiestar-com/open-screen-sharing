package accessibility

import (
	"context"
	"strings"
)

// Fractions of the interface height where the biometric screen places the
// button that opens the passcode page: Face ID centers it at 0.48 (24A446
// SpringBoardUIServices 0x1a7dae8ec..0x1a7dae970), Touch ID lays it out below
// its glyph and reason text, past the middle.
var unlockButtonBand = []float64{0.48, 0.55, 0.6, 0.52, 0.65, 0.44, 0.7, 0.4}

// AX coordinates are portrait-native whatever the interface orientation; the
// keypad is at the interface bottom. A button spanning most of the screen is a
// different one.
func (c *RemoteClient) FindUnlockButton(ctx context.Context, layout PasscodeLayout) (LayoutPoint, bool, error) {
	if !validShortcutFrame(layout.AppFrame) || layout.Keyboard == nil || !validShortcutFrame(layout.Keyboard.Frame) {
		return LayoutPoint{}, false, nil
	}
	screen, pad := layout.AppFrame, layout.Keyboard.Frame
	width, height := min(screen.Width, screen.Height), max(screen.Width, screen.Height)
	length, cross, bottom := height, width/2, pad.Y+pad.Height/2
	center := height / 2
	if screen.Width > screen.Height {
		length, cross, bottom = width, height/2, pad.X+pad.Width/2
		center = width / 2
	}
	down := bottom > center
	c.mu.Lock()
	defer c.mu.Unlock()
	var state PasscodeState
	app, values, raw, err := c.readPasscodeApplication(ctx, &state, true)
	if err != nil || values == nil {
		return LayoutPoint{}, false, err
	}
	data := raw.data
	if len(data) == 0 {
		response, err := c.query(ctx, requestAttribute, &app, attributeRawElementData, 0, nil)
		if err != nil {
			return LayoutPoint{}, false, err
		}
		if data = decodeLayoutRawElement(response.value); len(data) == 0 {
			return LayoutPoint{}, false, nil
		}
	}
	for _, fraction := range unlockButtonBand {
		along := fraction * length
		if !down {
			along = length - along
		}
		point := LayoutPoint{X: cross, Y: along}
		if screen.Width > screen.Height {
			point = LayoutPoint{X: along, Y: cross}
		}
		hit, code, err := c.readLayoutHit(ctx, app, point, data)
		if err != nil {
			return LayoutPoint{}, false, err
		}
		if code == nil || *code != 0 || hit == nil || !strings.HasSuffix(hit.Class, "Button") || !validShortcutFrame(hit.Frame) {
			continue
		}
		frame := hit.Frame
		if frame.Width*frame.Height > width*height/4 ||
			point.X <= frame.X || point.X >= frame.X+frame.Width || point.Y <= frame.Y || point.Y >= frame.Y+frame.Height {
			continue
		}
		return LayoutPoint{X: point.X / width, Y: point.Y / height}, true, nil
	}
	return LayoutPoint{}, false, nil
}
