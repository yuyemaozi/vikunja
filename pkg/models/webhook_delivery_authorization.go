// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package models

import (
	"fmt"
	"slices"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/user"
	"xorm.io/xorm"
)

// deliveryAuthorization returns a safe cancellation reason only when loss of
// authorization is proven. Database failures remain recoverable errors.
func deliveryAuthorization(s *xorm.Session, w *Webhook, record *WebhookDeliveryRecord) (string, error) {
	if !config.WebhooksEnabled.GetBool() {
		return "webhooks_disabled", nil
	}
	creator, err := user.GetUserByIDForAuth(s, w.CreatedByID)
	if user.IsErrUserDoesNotExist(err) || user.IsErrUserStatusError(err) || user.IsErrEmailNotConfirmed(err) {
		return "creator_inactive", nil
	}
	if err != nil {
		return "", err
	}
	if creator.Status != user.StatusActive {
		return "creator_inactive", nil
	}
	if !slices.Contains(w.Events, record.EventName) {
		return "subscription_removed", nil
	}
	if w.UserID > 0 && w.UserID != creator.ID {
		return "permission_revoked", nil
	}

	return deliverySnapshotAuthorization(s, w, record, creator)
}

func deliverySnapshotAuthorization(s *xorm.Session, w *Webhook, record *WebhookDeliveryRecord, creator *user.User) (string, error) {
	var envelope map[string]interface{}
	if len(record.PayloadBody) > 0 {
		if err := decodeEventPayload(record.PayloadBody, &envelope); err != nil {
			return "", errDeliveryPayloadCorrupt
		}
	} else if record.Payload != nil {
		// Legacy rows have not yet materialized their wire representation.
		envelope = map[string]interface{}{"data": record.Payload.Data}
	}
	if envelope == nil || envelope["event_name"] != record.EventName {
		return "", errDeliveryPayloadCorrupt
	}
	data, _ := envelope["data"].(map[string]interface{})
	projectID := w.ProjectID
	if project, ok := data["project"].(map[string]interface{}); ok {
		if id := getIDAsInt64(project["id"]); id > 0 {
			projectID = id
		}
	}
	if task, ok := data["task"].(map[string]interface{}); ok {
		if id := getIDAsInt64(task["project_id"]); id > 0 {
			projectID = id
		}
	}
	// A project-deletion snapshot was authorized before the object/ACL was removed.
	if projectID > 0 && record.EventName != (&ProjectDeletedEvent{}).Name() {
		project := &Project{ID: projectID}
		allowed, _, err := project.CanRead(s, creator)
		if IsErrProjectDoesNotExist(err) {
			return "permission_revoked", nil
		}
		if err != nil {
			return "", err
		}
		if !allowed {
			return "permission_revoked", nil
		}
	}
	tasks := []interface{}{}
	if task, ok := data["task"].(map[string]interface{}); ok {
		tasks = append(tasks, task)
	}
	if batch, ok := data["tasks"].([]interface{}); ok {
		tasks = append(tasks, batch...)
	}
	for _, value := range tasks {
		task, ok := value.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("invalid task authorization metadata")
		}
		id := getIDAsInt64(task["id"])
		if id <= 0 {
			return "", fmt.Errorf("missing task authorization metadata")
		}
		current := &Task{ID: id}
		allowed, _, err := current.CanRead(s, creator)
		if IsErrTaskDoesNotExist(err) && record.EventName == (&TaskDeletedEvent{}).Name() {
			continue
		}
		if IsErrTaskDoesNotExist(err) {
			return "permission_revoked", nil
		}
		if err != nil {
			return "", err
		}
		if !allowed {
			return "permission_revoked", nil
		}
	}
	return "", nil
}
