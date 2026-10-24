package accessibility

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/dtx"
	E "github.com/sagernet/sing/common/exceptions"
)

// AXPTranslatorRequest request types (24A446 AccessibilityPlatformTranslation,
// -[AXPTranslator processTranslatorRequest:] 0x24fe57a4c).
const (
	requestApplication = 1
	requestAttribute   = 2
	requestFrontmost   = 4
	requestAttributes  = 5
	requestHitTest     = 6
	requestAction      = 7
	requestTree        = 11
)

// AXPAttribute numbers (24A446 AccessibilityPlatformTranslation,
// __AXPAttributeToString 0x24fe585dc). In a requestAttribute request,
// attributeNative reads the native UIAccessibility attribute given as
// "parameter" (-[AXPTranslator_iOS processAttributeRequest:] 0x24fe4e4c8).
const (
	attributeBundleIdentifier = 6
	attributeChildren         = 8
	attributeCustomActions    = 13
	attributeFocusedElement   = 19
	attributeFrame            = 21
	attributeIdentifier       = 25
	attributeIsEnabled        = 27
	attributeIsValid          = 31
	attributeIsVisible        = 32
	attributeLabel            = 33
	attributeRawElementData   = 60
	attributeNative           = 63
	attributeTraits           = 77
)

const (
	nativeActiveKeyboard   = 2076
	nativeIsFirstResponder = 2198
	nativeFirstResponder   = 3002
	nativeClassName        = 5003
)

// 24A446 AXRuntime _kAXPlaysSoundTrait 0x18f8773f8, _kAXKeyboardKeyTrait
// 0x18f877400, _kAXSecureTextFieldTrait 0x18f877498.
const (
	traitPlaysSound      = 0x10
	traitKeyboardKey     = 0x20
	traitSecureTextField = 0x01000000
)

type RemoteClient struct {
	conn                    *dtx.Conn
	ctx                     context.Context
	owner                   context.Context
	cancel                  context.CancelCauseFunc
	stopContext             func() bool
	closeOnce               sync.Once
	closeErr                error
	enabled                 bool
	deviceClass             string
	mu                      sync.Mutex
	stateMu                 sync.Mutex
	pending                 *pendingRead
	readError               error
	done                    chan struct{}
	batchRawElementDisabled bool
	notifications           chan []translationObject
}

func DialRemote(ctx, owner context.Context, device *coredevice.Device) (_ *RemoteClient, err error) {
	// axauditd starts DTX on a remoteAXService connection without reading an
	// RSDCheckin (24B5110a axauditd 0x10000d214..0x10000d310).
	conn, err := device.DialService(ctx, "com.apple.accessibility.axAuditDaemon.remoteAXService")
	if err != nil {
		return nil, E.Cause(err, "accessibility: connect remoteAXService")
	}
	dtxConn, err := dtx.NewConn(owner, conn, func(event dtx.Event) (any, bool) {
		// The host side answers 7 (24A446 AccessibilityAudit
		// -[AXAuditRemoteDevice hostAPIVersion] 0x24fe0c4f0).
		if event.Selector == "hostAPIVersion" {
			return int64(7), true
		}
		if event.Selector == "_notifyOfPublishedCapabilities:" {
			return nil, true
		}
		return nil, false
	})
	if err != nil {
		return nil, E.Cause(err, "accessibility: initialize DTX")
	}
	operation, cancel := context.WithCancelCause(ctx)
	c := &RemoteClient{
		conn: dtxConn, ctx: operation, owner: owner, cancel: cancel,
		deviceClass: device.Info().DeviceClass, done: make(chan struct{}),
		notifications: make(chan []translationObject, 1),
	}
	c.mu.Lock()
	go c.readLoop()
	c.stopContext = context.AfterFunc(operation, func() { c.Close() })
	defer func() {
		c.mu.Unlock()
		if err != nil {
			err = E.Append(err, c.Close(), func(err error) error {
				return E.Cause(err, "accessibility: stop remote AX")
			})
		}
	}()
	if _, err = dtxConn.Invoke(operation, "deviceAPIVersion"); err != nil {
		return nil, E.Cause(err, "accessibility: invoke deviceAPIVersion")
	}
	if err = context.Cause(operation); err != nil {
		return nil, err
	}
	// AXAuditDevicesAppRemoteServer serves processDataFromHost: only after
	// clientNeedsAccessibility: true, which also starts pushing the cached tree
	// (24A446 AccessibilityAudit 0x24fe0b45c, 0x24fe0b5e4).
	c.enabled = true
	if err = dtxConn.Notify(owner, "clientNeedsAccessibility:", true); err != nil {
		return nil, E.Cause(err, "accessibility: notify clientNeedsAccessibility:")
	}
	if err = context.Cause(operation); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *RemoteClient) Close() error {
	c.closeOnce.Do(func() {
		c.cancel(net.ErrClosed)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopContext()
		// 24B5110a AXAuditDevicesAppRemoteServer clears its cache manager
		// only for clientNeedsAccessibility:false, then cancels DTX
		// (0x253f9c578..0x253f9c5a8). Socket loss (0x253f9bc3c, 0x253f9bf94)
		// leaves server -> manager -> _transportChannel -> server retained.
		if c.enabled {
			c.closeErr = c.conn.Notify(c.owner, "clientNeedsAccessibility:", false)
			if c.closeErr == nil {
				c.closeErr = c.conn.Wait()
				if errors.Is(c.closeErr, io.EOF) {
					c.closeErr = nil
				}
			}
		}
		c.conn.Close()
	})
	return c.closeErr
}

type PasscodeState struct {
	Ready       bool                `json:"ready"`
	Reason      string              `json:"reason"`
	Diagnostics PasscodeDiagnostics `json:"diagnostics"`
}

type PasscodeDiagnostics struct {
	Stage            string  `json:"stage"`
	RequestType      uint64  `json:"request_type"`
	Attribute        uint64  `json:"attribute,omitempty"`
	NativeAttribute  uint64  `json:"native_attribute,omitempty"`
	ResponseCode     *uint64 `json:"response_code"`
	SpringBoard      *bool   `json:"springboard,omitempty"`
	Candidate        string  `json:"candidate,omitempty"`
	Enabled          *uint64 `json:"enabled,omitempty"`
	Valid            *bool   `json:"valid,omitempty"`
	Visible          *bool   `json:"visible,omitempty"`
	Traits           *uint64 `json:"traits,omitempty"`
	Secure           *bool   `json:"secure,omitempty"`
	FirstResponder   *bool   `json:"first_responder,omitempty"`
	ResponderMatches *bool   `json:"responder_matches,omitempty"`
	FrontmostMatches *bool   `json:"frontmost_matches,omitempty"`
}

func (c *RemoteClient) ReadPasscodeState(ctx context.Context) (PasscodeState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var state PasscodeState
	app, values, _, err := c.readPasscodeApplication(ctx, &state, false)
	if err != nil || values == nil {
		return state, err
	}
	return c.readPasscodeField(ctx, state, app, values)
}

func (c *RemoteClient) ReadPasscodePreparation(ctx context.Context) (PasscodeState, *PasscodeLayout, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var state PasscodeState
	app, values, raw, err := c.readPasscodeApplication(ctx, &state, true)
	if err != nil || values == nil {
		return state, nil, err
	}
	state, err = c.readPasscodeField(ctx, state, app, values)
	if err != nil || state.Ready || state.Diagnostics.Stage == "frontmost_recheck" {
		return state, nil, err
	}
	layout, err := c.readPasscodeLayoutForApp(ctx, app, values, raw)
	return state, &layout, err
}

func (c *RemoteClient) readPasscodeQuery(ctx context.Context, state *PasscodeState, stage string, kind uint64, obj *translationObject, attribute, native uint64, params any) (remoteResponse, error) {
	state.Diagnostics.Stage = stage
	state.Diagnostics.RequestType = kind
	state.Diagnostics.Attribute = attribute
	state.Diagnostics.NativeAttribute = native
	state.Diagnostics.ResponseCode = nil
	state.Reason = stage + "_transport_error"
	response, err := c.query(ctx, kind, obj, attribute, 0, params)
	if err == nil {
		state.Diagnostics.ResponseCode = &response.code
		state.Reason = stage + "_response_error"
	}
	return response, err
}

func (c *RemoteClient) readPasscodeApplication(ctx context.Context, state *PasscodeState, includeFrame bool) (translationObject, map[any]any, layoutRawElement, error) {
	var raw layoutRawElement
	appResponse, err := c.readPasscodeQuery(ctx, state, "frontmost", requestFrontmost, nil, 0, 0, map[string]any{"displayId": int64(1)})
	if err != nil {
		return translationObject{}, nil, raw, err
	}
	if appResponse.code != 0 {
		return translationObject{}, nil, raw, nil
	}
	app, ok := parseTranslation(appResponse.value)
	if !ok || !app.application {
		state.Reason = "frontmost_invalid"
		return translationObject{}, nil, raw, nil
	}
	attributes := []any{int64(attributeBundleIdentifier), int64(attributeFocusedElement)}
	batchRaw := includeFrame && !c.batchRawElementDisabled
	if includeFrame {
		attributes = append(attributes, int64(attributeFrame))
		raw.status = "disabled"
		if batchRaw {
			attributes = append(attributes, int64(attributeRawElementData))
		}
	}
	// attributeFocusedElement reads nativeFirstResponder and, only when that is
	// nil, native 3027, the UIKit focus element (24A446
	// AccessibilityPlatformTranslation 0x24fe4c9a4..0x24fe4ca18).
	candidate, err := c.readPasscodeQuery(ctx, state, "candidate", requestAttributes, &app, 0, 0, map[string]any{"attributes": attributes})
	if err != nil {
		return translationObject{}, nil, raw, err
	}
	if batchRaw {
		code := candidate.code
		raw.code = &code
	}
	if batchRaw && candidate.code != 0 {
		// One failing attribute sets the error of the whole batch response (24B5110a).
		c.batchRawElementDisabled = true
		raw.status = "response_error"
		candidate, err = c.readPasscodeQuery(ctx, state, "candidate", requestAttributes, &app, 0, 0, map[string]any{"attributes": attributes[:len(attributes)-1]})
		if err != nil {
			return translationObject{}, nil, raw, err
		}
	}
	values, ok := candidate.value.(map[any]any)
	if candidate.code != 0 {
		return translationObject{}, nil, raw, nil
	}
	if !ok {
		state.Reason = "app_attributes_invalid"
		return translationObject{}, nil, raw, nil
	}
	springboard := values[uint64(attributeBundleIdentifier)] == "com.apple.springboard"
	state.Diagnostics.SpringBoard = &springboard
	if !springboard {
		state.Reason = "not_springboard"
		return translationObject{}, nil, raw, nil
	}
	if batchRaw && raw.status != "response_error" {
		value, present := values[uint64(attributeRawElementData)]
		raw.data = decodeLayoutRawElement(value)
		switch {
		case len(raw.data) != 0:
			raw.status = "valid"
		case !present || value == nil:
			raw.status = "missing"
		default:
			raw.status = "invalid"
		}
		if raw.status != "valid" {
			c.batchRawElementDisabled = true
		}
	}
	return app, values, raw, nil
}

func (c *RemoteClient) readPasscodeField(ctx context.Context, state PasscodeState, app translationObject, values map[any]any) (PasscodeState, error) {
	field, ok := parseTranslation(values[uint64(attributeFocusedElement)])
	switch {
	case values[uint64(attributeFocusedElement)] == nil:
		state.Diagnostics.Candidate = "missing"
	case !ok:
		state.Diagnostics.Candidate = "invalid"
	case field.application:
		state.Diagnostics.Candidate = "application"
	case field.pid != app.pid:
		state.Diagnostics.Candidate = "foreign_process"
	default:
		state.Diagnostics.Candidate = "element"
	}
	if state.Diagnostics.Candidate != "element" {
		state.Reason = "candidate_" + state.Diagnostics.Candidate
		return state, nil
	}
	attributes, err := c.readPasscodeQuery(ctx, &state, "field_attributes", requestAttributes, &field, 0, 0, map[string]any{"attributes": []any{
		int64(attributeIsEnabled), int64(attributeIsValid), int64(attributeIsVisible), int64(attributeTraits),
	}})
	if err != nil {
		return state, err
	}
	if attributes.code != 0 {
		return state, nil
	}
	if reason := inspectSecureFieldAttributes(attributes.value, &state.Diagnostics); reason != "" {
		state.Reason = reason
		return state, nil
	}
	// nativeIsFirstResponder asks the nearest self-or-ancestor UIResponder
	// (24A446 AccessibilityPlatformTranslation 0x24fe4ca54).
	responder, err := c.readPasscodeQuery(ctx, &state, "first_responder", requestAttribute, &field, attributeNative, nativeIsFirstResponder, map[string]any{"parameter": int64(nativeIsFirstResponder)})
	if err != nil {
		return state, err
	}
	if responder.code != 0 {
		return state, nil
	}
	firstResponder, ok := responder.value.(bool)
	if !ok {
		state.Reason = "first_responder_unavailable"
		return state, nil
	}
	state.Diagnostics.FirstResponder = &firstResponder
	if !firstResponder {
		state.Reason = "not_first_responder"
		return state, nil
	}
	// Requests share no UI snapshot, so the field read above may already be
	// replaced (24A446).
	recheck, err := c.readPasscodeQuery(ctx, &state, "responder_recheck", requestAttribute, &app, attributeNative, nativeFirstResponder, map[string]any{"parameter": int64(nativeFirstResponder)})
	if err != nil {
		return state, err
	}
	current, ok := parseTranslation(recheck.value)
	if recheck.code != 0 {
		return state, nil
	}
	if !ok {
		state.Reason = "responder_recheck_unavailable"
		return state, nil
	}
	responderMatches := field.same(current)
	state.Diagnostics.ResponderMatches = &responderMatches
	if !responderMatches {
		state.Reason = "responder_changed"
		return state, nil
	}
	frontmost, err := c.readPasscodeQuery(ctx, &state, "frontmost_recheck", requestFrontmost, nil, 0, 0, map[string]any{"displayId": int64(1)})
	if err != nil {
		return state, err
	}
	currentApp, ok := parseTranslation(frontmost.value)
	if frontmost.code != 0 {
		return state, nil
	}
	if !ok {
		state.Reason = "frontmost_recheck_unavailable"
		return state, nil
	}
	frontmostMatches := app.same(currentApp)
	state.Diagnostics.FrontmostMatches = &frontmostMatches
	state.Ready = frontmostMatches
	state.Reason = "frontmost_changed"
	if state.Ready {
		state.Reason = "ready"
	}
	return state, nil
}

type translationObject struct {
	pid         uint64
	objectID    uint64
	application bool
	fields      map[string]any
}

func parseTranslation(value any) (translationObject, bool) {
	fields, ok := value.(map[string]any)
	if !ok || fields["$class"] != "AXPTranslationObject" {
		return translationObject{}, false
	}
	pid, pidOK := unsigned(fields["pid"])
	id, idOK := unsigned(fields["objectID"])
	application, appOK := fields["isApplicationElement"].(bool)
	return translationObject{pid: pid, objectID: id, application: application, fields: fields}, pidOK && idOK && appOK && pid != 0 && id != 0
}

func (t translationObject) same(other translationObject) bool {
	return t.pid == other.pid && t.objectID == other.objectID && t.application == other.application
}

// -[AXPTranslationObject encodeWithCoder:] spells the key didPopuldateAppInfo
// (24A446 AccessibilityPlatformTranslation 0x24fe58b20).
func (t translationObject) archiveObject() dtx.Object {
	fields := make(map[string]any)
	for _, key := range []string{"pid", "objectID", "isApplicationElement", "didPopuldateAppInfo", "bridgeDelegateToken", "rawElementData"} {
		if value, ok := t.fields[key]; ok {
			fields[key] = value
		}
	}
	return dtx.Object{ClassName: "AXPTranslationObject", Fields: fields, PrimitiveFields: []string{"pid", "isApplicationElement", "didPopuldateAppInfo"}}
}

type remoteResponse struct {
	value any
	code  uint64
}

// AXP responses carry the request type but no identifier (24A446
// AccessibilityPlatformTranslation -[AXPTranslatorResponse encodeWithCoder:]
// 0x24fe59764).
func (c *RemoteClient) query(ctx context.Context, kind uint64, obj *translationObject, attribute, action uint64, params any) (response remoteResponse, err error) {
	var empty remoteResponse
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := context.Cause(c.ctx); err != nil {
		return empty, err
	}
	stop := context.AfterFunc(ctx, func() {
		c.cancel(context.Cause(ctx))
		c.Close()
	})
	defer func() {
		stop()
		if cause := context.Cause(c.ctx); cause != nil {
			response, err = empty, cause
		}
		if ctx.Err() != nil {
			c.cancel(context.Cause(ctx))
			go c.Close()
			response, err = empty, ctx.Err()
		}
	}()
	var translation any
	if obj != nil {
		translation = obj.archiveObject()
	} else if kind == requestFrontmost {
		translation = dtx.Object{ClassName: "AXPTranslationObject", Fields: map[string]any{
			"pid": int64(0), "objectID": int64(0), "isApplicationElement": false, "didPopuldateAppInfo": false,
		}, PrimitiveFields: []string{"pid", "isApplicationElement", "didPopuldateAppInfo"}}
	}
	data, err := dtx.MarshalArchive(dtx.Object{ClassName: "AXPTranslatorRequest", Fields: map[string]any{
		"requestType": kind, "attributeType": attribute, "actionType": action, "clientType": int64(2),
		"translation": translation, "parameters": params,
	}, PrimitiveFields: []string{"requestType", "attributeType", "actionType", "clientType"}})
	if err != nil {
		return empty, err
	}
	read := &pendingRead{kind: kind, attribute: attribute, response: make(chan remoteResponse, 1)}
	c.stateMu.Lock()
	c.pending = read
	c.stateMu.Unlock()
	defer func() {
		c.stateMu.Lock()
		c.pending = nil
		c.stateMu.Unlock()
	}()
	if err = c.conn.Notify(c.owner, "processDataFromHost:", data); err != nil {
		return empty, err
	}
	select {
	case response := <-read.response:
		return response, nil
	case <-ctx.Done():
		return empty, ctx.Err()
	case <-c.ctx.Done():
		return empty, context.Cause(c.ctx)
	case <-c.done:
		c.stateMu.Lock()
		err := c.readError
		c.stateMu.Unlock()
		return empty, err
	}
}

type pendingRead struct {
	kind      uint64
	attribute uint64
	response  chan remoteResponse
}

func (c *RemoteClient) readLoop() {
	defer close(c.done)
	for {
		event, err := c.conn.Receive(context.Background())
		if err == nil {
			err = c.receive(event)
		}
		if err != nil {
			c.stateMu.Lock()
			c.readError = err
			c.stateMu.Unlock()
			c.cancel(err)
			return
		}
	}
}

func (c *RemoteClient) receive(event dtx.Event) error {
	if event.Selector != "processDataFromRemoteDevice:" {
		return nil
	}
	for _, argument := range event.Arguments {
		data, ok := argument.([]byte)
		if !ok {
			return E.New("accessibility: remote AX response is not archived data")
		}
		decoded, err := dtx.UnmarshalArchive(data)
		if err != nil {
			return err
		}
		response, ok := decoded.(map[string]any)
		if !ok || response["$class"] != "AXPTranslatorResponse" {
			return E.New("accessibility: invalid remote AX response")
		}
		kind, kindOK := unsigned(response["associatedRequestType"])
		attribute, attrOK := unsigned(response["attribute"])
		code, codeOK := unsigned(response["error"])
		notification, notificationOK := unsigned(response["notification"])
		if !kindOK || !attrOK || !codeOK || !notificationOK {
			return E.New("accessibility: invalid remote AX response metadata")
		}
		if notification != 0 {
			continue
		}
		if kind == requestTree && code == 0 {
			c.receiveScreenViewingNotifications(response["resultData"])
			continue
		}
		c.stateMu.Lock()
		pending := c.pending
		if pending != nil && pending.kind == kind && (kind != requestAttribute || pending.attribute == attribute) {
			select {
			case pending.response <- remoteResponse{value: response["resultData"], code: code}:
			default:
				c.stateMu.Unlock()
				return E.New("accessibility: duplicate remote AX response")
			}
		}
		c.stateMu.Unlock()
	}
	return nil
}

func inspectSecureFieldAttributes(value any, diagnostics *PasscodeDiagnostics) string {
	values, ok := value.(map[any]any)
	if !ok {
		return "field_attributes_invalid"
	}
	attributes := make(map[uint64]any)
	for key, value := range values {
		if number, ok := unsigned(key); ok {
			attributes[number] = value
		}
	}
	enabled, enabledOK := unsigned(attributes[attributeIsEnabled])
	traits, traitsOK := unsigned(attributes[attributeTraits])
	valid, validOK := attributes[attributeIsValid].(bool)
	visible, visibleOK := attributes[attributeIsVisible].(bool)
	if enabledOK {
		diagnostics.Enabled = &enabled
	}
	if validOK {
		diagnostics.Valid = &valid
	}
	if visibleOK {
		diagnostics.Visible = &visible
	}
	secure := traits&traitSecureTextField != 0
	if traitsOK {
		diagnostics.Traits = &traits
		diagnostics.Secure = &secure
	}
	switch {
	case !enabledOK:
		return "enabled_unavailable"
	case enabled != 1:
		return "not_enabled"
	case !validOK:
		return "validity_unavailable"
	case !valid:
		return "invalid_field"
	case !visibleOK:
		return "visibility_unavailable"
	case !visible:
		return "invisible_field"
	case !traitsOK:
		return "traits_unavailable"
	case !secure:
		return "not_secure"
	default:
		return ""
	}
}

func unsigned(value any) (uint64, bool) {
	switch number := value.(type) {
	case uint64:
		return number, true
	case int64:
		return uint64(number), number >= 0
	case uint32:
		return uint64(number), true
	case int:
		return uint64(number), number >= 0
	default:
		return 0, false
	}
}
