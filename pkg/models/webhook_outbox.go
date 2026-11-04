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
	"sync"

	"code.vikunja.io/api/pkg/events"
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
	_ any,
	_ events.PreparedEvent,
	_ events.RequestMeta,
) ([]events.Event, error) {
	// Task 2B replaces this with StageWebhookDeliveries.
	return nil, nil
}

// WebhookSourceEventNames returns the catalogue in registration order.
func WebhookSourceEventNames() []string {
	names := make([]string, 0, len(webhookSourceEvents))
	for _, event := range webhookSourceEvents {
		names = append(names, event.Name())
	}
	return names
}
