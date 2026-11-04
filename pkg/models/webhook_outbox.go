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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/user"

	"github.com/google/uuid"
	"xorm.io/xorm"
)

// webhookSourceEvents is the closed catalogue of events that must stage
// delivery snapshots inside the business transaction.
var webhookSourceEvents = []events.Event{
	&TaskCreatedEvent{},
	&TaskUpdatedEvent{},
	&TaskDeletedEvent{},
	&TaskAssigneeCreatedEvent{},
	&TaskAssigneeDeletedEvent{},
	&TaskCommentCreatedEvent{},
	&TaskCommentUpdatedEvent{},
	&TaskCommentDeletedEvent{},
	&TaskAttachmentCreatedEvent{},
	&TaskAttachmentDeletedEvent{},
	&TaskRelationCreatedEvent{},
	&TaskRelationDeletedEvent{},
	&ProjectUpdatedEvent{},
	&ProjectDeletedEvent{},
	&ProjectSharedWithUserEvent{},
	&ProjectSharedWithTeamEvent{},
	&TaskReminderFiredEvent{},
	&TaskOverdueEvent{},
	&TasksOverdueEvent{},
}

var webhookOutboxOnce sync.Once
var webhookOutboxErr error

// RegisterWebhookOutbox registers the transactional sink for all webhook
// sources. It is idempotent and must run before any write path starts.
func RegisterWebhookOutbox() error {
	webhookOutboxOnce.Do(func() {
		names := make([]string, 0, len(webhookSourceEvents))
		for _, event := range webhookSourceEvents {
			names = append(names, event.Name())
		}
		webhookOutboxErr = events.RegisterTransactionalSink(names, stageWebhookDeliveries)
	})
	return webhookOutboxErr
}

func stageWebhookDeliveries(
	key any,
	prepared events.PreparedEvent,
	meta events.RequestMeta,
) ([]events.Event, error) {
	if !config.WebhooksEnabled.GetBool() {
		return nil, nil
	}
	session, ok := key.(*xorm.Session)
	if !ok || !session.IsInTx() {
		return nil, events.ErrWebhookTransactionRequired
	}
	return StageWebhookDeliveries(session, prepared, meta.RequestID)
}

// StageWebhookDeliveries persists one final snapshot per matching webhook inside
// the caller's transaction and returns the delivery wake-up events.
func StageWebhookDeliveries(
	s *xorm.Session,
	prepared events.PreparedEvent,
	requestID string,
) ([]events.Event, error) {
	var event map[string]interface{}
	if err := json.Unmarshal(prepared.Payload, &event); err != nil {
		return nil, err
	}

	projectID, err := getProjectIDForEvent(s, event)
	if err != nil {
		return nil, err
	}
	isUserDirected := IsUserDirectedEvent(prepared.Name)
	if projectID == 0 && !isUserDirected {
		return nil, nil
	}

	matchingWebhooks, err := projectWebhooksForEvent(s, projectID, prepared.Name)
	if err != nil {
		return nil, err
	}
	if isUserDirected {
		userID := getUserIDFromAnyEvent(event)
		if userID > 0 {
			userWebhooks := []*Webhook{}
			if err := s.Where("user_id = ? AND (project_id IS NULL OR project_id = 0)", userID).
				OrderBy("id ASC").
				Find(&userWebhooks); err != nil {
				return nil, err
			}
			for _, webhook := range userWebhooks {
				for _, name := range webhook.Events {
					if name == prepared.Name {
						matchingWebhooks = append(matchingWebhooks, webhook)
						break
					}
				}
			}
		}
	}
	if len(matchingWebhooks) == 0 {
		return nil, nil
	}

	event, doerID, err := reloadEventData(s, event, projectID)
	if err != nil {
		return nil, err
	}

	results := make([]events.Event, 0, len(matchingWebhooks))
	for _, webhook := range matchingWebhooks {
		perWebhookEvent := make(map[string]interface{}, len(event)+1)
		for key, value := range event {
			perWebhookEvent[key] = value
		}

		if doerID != 0 {
			canRead, err := reloadTaskInEvent(s, perWebhookEvent, webhook.CreatedByID)
			if err != nil {
				return nil, err
			}
			if !canRead {
				continue
			}
		}

		if _, has := perWebhookEvent["project"]; !has && webhook.ProjectID > 0 {
			project, err := GetProjectSimpleByID(s, webhook.ProjectID)
			if err != nil && !IsErrProjectDoesNotExist(err) {
				return nil, err
			}
			if project != nil {
				err = project.ReadOne(s, &user.User{ID: doerID})
				if err != nil && !IsErrProjectDoesNotExist(err) {
					return nil, err
				}
				if err == nil {
					perWebhookEvent["project"] = project
				}
			}
		}

		payload := &WebhookPayload{
			EventName: prepared.Name,
			Time:      prepared.OccurredAt,
			Data:      perWebhookEvent,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		deliveryID := uuid.NewSHA1(
			uuid.NameSpaceOID,
			[]byte(fmt.Sprintf("%s:%d:%s", prepared.ID, webhook.ID, prepared.Name)),
		).String()

		record := &WebhookDeliveryRecord{
			DeliveryID:    deliveryID,
			WebhookID:     webhook.ID,
			EventName:     prepared.Name,
			Payload:       payload,
			Status:        WebhookDeliveryStatusPending,
			RequestID:     requestID,
			EventID:       prepared.ID,
			PayloadBody:   body,
			PayloadSHA256: hex.EncodeToString(sum[:]),
			CreatedAt:     time.Now().UTC(),
		}
		if _, err := s.Insert(record); err != nil {
			return nil, err
		}
		results = append(results, &WebhookDeliveryEvent{DeliveryID: deliveryID})
	}
	return results, nil
}

// WebhookSourceEventNames returns the catalogue in registration order.
func WebhookSourceEventNames() []string {
	names := make([]string, 0, len(webhookSourceEvents))
	for _, event := range webhookSourceEvents {
		names = append(names, event.Name())
	}
	return names
}
