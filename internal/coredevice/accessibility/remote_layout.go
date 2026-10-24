package accessibility

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/dtx"
)

type LayoutFrame struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type LayoutPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type LayoutElement struct {
	Class string       `json:"class"`
	Frame *LayoutFrame `json:"frame"`
}

type PasscodeLayout struct {
	Status              string         `json:"status"`
	SpringBoard         bool           `json:"springboard"`
	FrontmostConsistent bool           `json:"frontmost_consistent"`
	AppFrame            *LayoutFrame   `json:"app_frame"`
	Field               *LayoutElement `json:"field"`
	Keyboard            *LayoutElement `json:"keyboard"`
	Hit                 *LayoutElement `json:"hit"`
	HitPoint            *LayoutPoint   `json:"hit_point"`
	HitCode             *uint64        `json:"hit_code"`
	RawElementSource    string         `json:"raw_element_source,omitempty"`
	RawElementBatch     string         `json:"raw_element_batch,omitempty"`
	RawElementBatchCode *uint64        `json:"raw_element_batch_code,omitempty"`
	RawElementCode      *uint64        `json:"raw_element_code,omitempty"`
}

// requestAttribute and requestAttributes read attributeRawElementData through
// the same handler and return the same token (24A446
// AccessibilityPlatformTranslation 0x24fe4d3b0); a batch token passed HitTest
// on iPad17,1 / 24B5110a.
type layoutRawElement struct {
	data   []byte
	status string
	code   *uint64
}

func (c *RemoteClient) readPasscodeLayoutForApp(ctx context.Context, app translationObject, values map[any]any, raw layoutRawElement) (PasscodeLayout, error) {
	out := PasscodeLayout{Status: "reading", SpringBoard: true, AppFrame: decodeLayoutFrame(values[uint64(attributeFrame)])}
	var err error
	if field, ok := parseTranslation(values[uint64(attributeFocusedElement)]); ok && !field.application && field.pid == app.pid {
		out.Field, err = c.readLayoutElement(ctx, field)
		if err != nil {
			return out, err
		}
	}
	// nativeActiveKeyboard returns the active keyboard, else SpringBoard's
	// search for an SBUIPasscodeLockNumberPad while the passcode lock is visible
	// (24A446 UIAccessibility 0x1bb73be64, SpringBoard AX 0x2248c9098).
	keyboard, err := c.query(ctx, requestAttribute, &app, attributeNative, 0, map[string]any{"parameter": int64(nativeActiveKeyboard)})
	if err != nil {
		return out, err
	}
	if obj, ok := parseTranslation(keyboard.value); keyboard.code == 0 && ok && !obj.application && obj.pid == app.pid {
		out.Keyboard, err = c.readLayoutElement(ctx, obj)
		if err != nil {
			return out, err
		}
	}
	if frame := out.AppFrame; frame != nil && frame.Width > 0 && frame.Height > 0 {
		// On iPhone18,2 this is inside the Face ID button and 18 pt above the
		// keypad's first hit row (24A446).
		point := LayoutPoint{X: frame.X + frame.Width*0.5, Y: frame.Y + frame.Height*0.325}
		if c.deviceClass == "iPad" {
			// The biometric button is centered on the screen whatever the
			// interface orientation; hit tests use the portrait-native space (24A446).
			point = LayoutPoint{X: min(frame.Width, frame.Height) / 2, Y: max(frame.Width, frame.Height) / 2}
		}
		if finiteLayoutNumber(point.X) && finiteLayoutNumber(point.Y) {
			out.HitPoint = &point
			out.RawElementBatch = raw.status
			out.RawElementBatchCode = raw.code
			out.RawElementSource = "batch"
			data := raw.data
			code := uint64(0)
			if raw.status != "valid" {
				out.RawElementSource = "single"
				response, readErr := c.query(ctx, requestAttribute, &app, attributeRawElementData, 0, nil)
				if readErr != nil {
					return out, readErr
				}
				code = response.code
				if code == 0 {
					data = decodeLayoutRawElement(response.value)
				}
			}
			out.RawElementCode = &code
			if len(data) != 0 {
				out.Hit, out.HitCode, err = c.readLayoutHit(ctx, app, point, data)
				if err != nil {
					return out, err
				}
			}
		}
	}
	front, err := c.query(ctx, requestFrontmost, nil, 0, 0, map[string]any{"displayId": int64(1)})
	if err != nil {
		return out, err
	}
	current, ok := parseTranslation(front.value)
	out.FrontmostConsistent = front.code == 0 && ok && app.same(current)
	out.Status = "ok"
	if !out.FrontmostConsistent {
		out.Status = "frontmost_changed"
	}
	return out, nil
}

func (c *RemoteClient) readLayoutElement(ctx context.Context, obj translationObject) (*LayoutElement, error) {
	out := &LayoutElement{}
	// nativeClassName returns NSStringFromClass of the backing object unless a
	// class overrides it (24A446 UIAccessibility 0x1bb73b2b0, 0x1bb7655f0). The
	// AXP class name attribute is unsupported on iOS; native 2185, the
	// description, can include text.
	class, err := c.query(ctx, requestAttribute, &obj, attributeNative, 0, map[string]any{"parameter": int64(nativeClassName)})
	if err != nil {
		return nil, err
	}
	if class.code == 0 {
		out.Class = layoutClassName(class.value)
	}
	frame, err := c.query(ctx, requestAttribute, &obj, attributeFrame, 0, nil)
	if err != nil {
		return nil, err
	}
	if frame.code == 0 {
		out.Frame = decodeLayoutFrame(frame.value)
	}
	return out, nil
}

func decodeLayoutRawElement(value any) []byte {
	token, ok := value.(map[string]any)
	if !ok || token["TokenType"] != "AXElementTokenSimulator" {
		return nil
	}
	data, ok := token["ElementData"].([]byte)
	if !ok || len(data) == 0 || len(data) > 4096 {
		return nil
	}
	return data
}

func (c *RemoteClient) readLayoutHit(ctx context.Context, app translationObject, point LayoutPoint, data []byte) (*LayoutElement, *uint64, error) {
	// Translations are created without rawElementData, and HitTest decodes the
	// element from it, even for an application translation (24A446
	// AccessibilityPlatformTranslation 0x24fe51328, 0x24fe50e34..0x24fe50e40).
	fields := maps.Clone(app.fields)
	fields["rawElementData"] = data
	app.fields = fields
	value := dtx.Object{ClassName: "NSValue", Fields: map[string]any{
		"NS.special":  int64(1),
		"NS.pointval": "{" + strconv.FormatFloat(point.X, 'g', -1, 64) + ", " + strconv.FormatFloat(point.Y, 'g', -1, 64) + "}",
	}, PrimitiveFields: []string{"NS.special"}}
	hit, err := c.query(ctx, requestHitTest, &app, 0, 0, map[string]any{"point": value, "displayId": int64(1), "contextId": int64(0)})
	if err != nil {
		return nil, nil, err
	}
	obj, ok := parseTranslation(hit.value)
	if hit.code != 0 || !ok || obj.application || obj.pid != app.pid {
		return nil, &hit.code, nil
	}
	element, err := c.readLayoutElement(ctx, obj)
	return element, &hit.code, err
}

func layoutClassName(value any) string {
	s, ok := value.(string)
	if !ok || len(s) == 0 || len(s) > 256 {
		return ""
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._$", r) {
			return ""
		}
	}
	return s
}

func decodeLayoutFrame(value any) *LayoutFrame {
	fields, ok := value.(map[string]any)
	if !ok || fields["$class"] != "NSValue" {
		return nil
	}
	s, ok := fields["NS.rectval"].(string)
	if !ok || len(s) > 256 {
		return nil
	}
	var frame LayoutFrame
	if n, err := fmt.Sscanf(s, "{{%f, %f}, {%f, %f}}", &frame.X, &frame.Y, &frame.Width, &frame.Height); err != nil || n != 4 {
		return nil
	}
	if !finiteLayoutNumber(frame.X) || !finiteLayoutNumber(frame.Y) || !finiteLayoutNumber(frame.Width) || !finiteLayoutNumber(frame.Height) || frame.Width < 0 || frame.Height < 0 {
		return nil
	}
	return &frame
}

func finiteLayoutNumber(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
