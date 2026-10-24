package accessibility

import (
	"context"
	"slices"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// dtremotedisplayd 27B5028f supplies this body for its screen-viewing audit
// notification. SpringBoard 24B5110a prefixes its AX label with Device Hub.
func screenViewingNotification(label string) bool {
	source, _, found := strings.Cut(label, ", ")
	return found && strings.EqualFold(source, "Device Hub") &&
		strings.HasSuffix(label, ", This device was viewed remotely from Mac")
}

func (c *RemoteClient) receiveScreenViewingNotifications(value any) {
	data, ok := value.(map[string]any)
	if !ok {
		return
	}
	tree, _ := data["treeDump"].([]any)
	var candidates []translationObject
	for _, item := range tree {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		label := ""
		if attribute, _ := unsigned(entry["attribute"]); attribute == attributeLabel {
			label, _ = entry["resultData"].(string)
		} else if values, ok := entry["resultData"].(map[any]any); ok {
			label, _ = values[uint64(attributeLabel)].(string)
		}
		obj, ok := parseTranslation(entry["associatedTranslationObject"])
		if ok && !obj.application && screenViewingNotification(label) {
			candidates = append(candidates, obj)
		}
	}
	if len(candidates) == 0 {
		return
	}
	select {
	case pending := <-c.notifications:
		for _, obj := range pending {
			if !slices.ContainsFunc(candidates, obj.same) {
				candidates = append(candidates, obj)
			}
		}
	default:
	}
	c.notifications <- candidates
}

func (c *RemoteClient) DismissScreenViewingNotifications(ctx context.Context) (int, error) {
	var candidates []translationObject
	select {
	case candidates = <-c.notifications:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.ctx.Done():
		return 0, context.Cause(c.ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	count := 0
	for _, obj := range candidates {
		dismissed, err := c.dismissScreenViewingNotification(ctx, obj)
		if err != nil {
			return count, err
		}
		if dismissed {
			count++
		}
	}
	return count, nil
}

func (c *RemoteClient) dismissScreenViewingNotification(ctx context.Context, obj translationObject) (bool, error) {
	application, err := c.query(ctx, requestApplication, nil, 0, 0, map[string]any{"pid": obj.pid})
	if err != nil {
		return false, err
	}
	app, ok := parseTranslation(application.value)
	if application.code != 0 || !ok || !app.application || app.pid != obj.pid {
		return false, nil
	}
	bundle, err := c.query(ctx, requestAttribute, &app, attributeBundleIdentifier, 0, nil)
	if err != nil {
		return false, err
	}
	if bundle.code != 0 || bundle.value != "com.apple.springboard" {
		return false, nil
	}
	response, err := c.query(ctx, requestAttributes, &obj, 0, 0, map[string]any{
		"attributes": []any{int64(attributeLabel), int64(attributeIdentifier), int64(attributeIsValid), int64(attributeCustomActions)},
	})
	if err != nil {
		return false, err
	}
	values, ok := response.value.(map[any]any)
	if response.code != 0 || !ok || values[uint64(attributeIsValid)] != true || values[uint64(attributeIdentifier)] != "ListCell" {
		return false, nil
	}
	label, _ := values[uint64(attributeLabel)].(string)
	if !screenViewingNotification(label) {
		return false, nil
	}
	actions, _ := values[uint64(attributeCustomActions)].([]any)
	identifier := ""
	for _, value := range actions {
		action, ok := value.(map[string]any)
		if !ok {
			continue
		}
		id, _ := action["AXPCustomActionIdentifier"].(string)
		// SpringBoard's notification Clear action is block-backed; More uses
		// _axMore:. AXP serializes the block action with no target/selector,
		// retaining its localized name (24B5110a).
		if strings.HasSuffix(id, "\nTarget:0x0\nSelector:(null)") {
			if identifier != "" {
				return false, nil
			}
			identifier = id
		}
	}
	if identifier == "" {
		return false, nil
	}
	// AXP action 4 performs the custom action selected by this identifier
	// (24A446 AccessibilityPlatformTranslation 0x24fe48264). Generic supported
	// actions omitted 4 on 24B5110a despite this action executing successfully.
	response, err = c.query(ctx, requestAction, &obj, 0, 4, map[string]any{"AXPCustomActionIdentifier": identifier})
	if err != nil {
		return false, err
	}
	if response.code != 0 {
		return false, E.New("accessibility: clear screen-viewing notification: response ", response.code)
	}
	dismissed, _ := response.value.(bool)
	return dismissed, nil
}
