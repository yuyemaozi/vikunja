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
	"encoding/json"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookRegistryHasExactly19Sources(t *testing.T) {
	names := WebhookSourceEventNames()

	assert.Len(t, names, 19)
	seen := map[string]bool{}
	for _, name := range names {
		assert.False(t, seen[name], "duplicate source %s", name)
		seen[name] = true
	}
}

func TestRegisterWebhookOutboxIsIdempotent(t *testing.T) {
	assert.NoError(t, RegisterWebhookOutbox())
	assert.NoError(t, RegisterWebhookOutbox())
}

func TestTransactionalSinkBeforeEnqueue(t *testing.T) {
	db.LoadAndAssertFixtures(t)

	payload, err := json.Marshal(&TaskUpdatedEvent{
		Task: &Task{ID: 1, ProjectID: 1},
		Doer: &user.User{ID: 1},
	})
	require.NoError(t, err)

	const eventID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
	prepared := events.PreparedEvent{
		ID:         eventID,
		Name:       "task.updated",
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}

	s := db.NewSession()
	followUps, err := StageWebhookDeliveries(s, prepared, "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b")
	require.NoError(t, err)
	require.NotEmpty(t, followUps)

	visible, err := s.Where("event_id = ?", eventID).Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	require.Positive(t, visible)

	require.NoError(t, s.Rollback())
	_ = s.Close()

	after := db.NewSession()
	count, err := after.Where("event_id = ?", eventID).Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	assert.Zero(t, count)
	_ = after.Close()
}

func TestWebhookDisabledDoesNotStage(t *testing.T) {
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(false)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })

	db.LoadAndAssertFixtures(t)
	payload, err := json.Marshal(&TaskUpdatedEvent{
		Task: &Task{ID: 1, ProjectID: 1},
		Doer: &user.User{ID: 1},
	})
	require.NoError(t, err)

	s := db.NewSession()
	defer s.Close()
	_, err = stageWebhookDeliveries(s, events.PreparedEvent{
		ID:         "018f8f4c-2e18-7b61-9a50-9c4f5de4e10c",
		Name:       "task.updated",
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}, events.RequestMeta{})
	require.NoError(t, err)

	count, err := s.Where("event_id = ?", "018f8f4c-2e18-7b61-9a50-9c4f5de4e10c").
		Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestWebhookSinkReadyWithoutAsync(t *testing.T) {
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(true)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })

	require.NoError(t, RegisterWebhookOutbox())
	err := events.DispatchOnCommit(new(int), &TaskUpdatedEvent{
		Task: &Task{ID: 1, ProjectID: 1},
		Doer: &user.User{ID: 1},
	})
	assert.ErrorIs(t, err, events.ErrWebhookTransactionRequired)
}

func TestNewOutboxRejectsInvalidRequestID(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	payload, err := json.Marshal(&TaskUpdatedEvent{
		Task: &Task{ID: 1, ProjectID: 1},
		Doer: &user.User{ID: 1},
	})
	require.NoError(t, err)

	s := db.NewSession()
	defer s.Close()
	_, err = StageWebhookDeliveries(s, events.PreparedEvent{
		ID:         "018f8f4c-2e18-7b61-9a50-9c4f5de4e10d",
		Name:       "task.updated",
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}, "")
	assert.Error(t, err)
}
