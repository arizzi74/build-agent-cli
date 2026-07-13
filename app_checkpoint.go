package main

import (
	"context"
	"net/url"
	"strings"
)

// PatchAppCreatedCheckpoint mirrors the Web UI mutation that annotates the
// original user message after create_new_servicenow_app succeeds.
func (c *Client) PatchAppCreatedCheckpoint(ctx context.Context, messageSysID string, original RichUserContent, appSysID, appName string) error {
	messageSysID = strings.TrimSpace(messageSysID)
	appSysID = strings.TrimSpace(appSysID)
	if messageSysID == "" || appSysID == "" {
		return nil
	}
	original.HasCheckpoints = true
	checkpointID := "APP_CREATED:" + appSysID
	found := false
	for i := range original.Checkpoints {
		if original.Checkpoints[i].ID == checkpointID {
			original.Checkpoints[i].AppDir = appName
			found = true
		}
	}
	if !found {
		original.Checkpoints = append(original.Checkpoints, RichMessageCheckpoint{ID: checkpointID, AppDir: appName})
	}
	checkpoint, err := richMessageContentJSONString(original)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.cfg.InstanceURL, "/") + richWebMessageEndpointPrefix +
		url.PathEscape(c.conversationID) + "/messages/" + url.PathEscape(messageSysID)
	_, _, err = c.patchJSON(ctx, endpoint, map[string]interface{}{"content": checkpoint})
	return err
}
