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
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
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

func TestRequestMetaBoundBeforeSnapshot(t *testing.T) {
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(true)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())

	const requestID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10e"
	s := db.NewSession()
	defer s.Close()
	defer events.CleanupPending(s)

	events.BindRequestMeta(
		s,
		events.WithRequestMeta(context.Background(), &events.RequestMeta{RequestID: requestID}),
	)
	require.NoError(t, events.DispatchOnCommit(s, &TaskUpdatedEvent{
		Task: &Task{ID: 1, ProjectID: 1},
		Doer: &user.User{ID: 1},
	}))

	records := []*WebhookDeliveryRecord{}
	require.NoError(t, s.OrderBy("id DESC").Limit(1).Find(&records))
	require.NotEmpty(t, records)
	assert.Equal(t, requestID, records[0].RequestID)
	require.NoError(t, s.Rollback())
}

func TestTaskCreateRollsBackWhenSnapshotFails(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())

	s := db.NewSession()
	defer s.Close()
	for index := 0; index < 2; index++ {
		webhook := &Webhook{
			TargetURL:   "https://example.com/hook",
			Events:      []string{"task.created"},
			ProjectID:   1,
			CreatedByID: 1,
		}
		_, err := s.Insert(webhook)
		require.NoError(t, err)
	}
	events.BindRequestMeta(
		s,
		events.WithRequestMeta(context.Background(), &events.RequestMeta{
			RequestID: "018f8f4c-2e18-7b61-9a50-9c4f5de4e50b",
		}),
	)
	defer events.CleanupPending(s)

	previous := insertDeliverySnapshot
	calls := 0
	insertDeliverySnapshot = func(session *xorm.Session, record *WebhookDeliveryRecord) error {
		calls++
		if calls == 2 {
			return errors.New("injected snapshot failure")
		}
		return previous(session, record)
	}
	t.Cleanup(func() { insertDeliverySnapshot = previous })

	task := &Task{Title: "rollback snapshot", ProjectID: 1}
	err := task.Create(s, &user.User{ID: 1})
	require.Error(t, err)
	require.NoError(t, s.Rollback())

	check := db.NewSession()
	defer check.Close()
	taskCount, err := check.Where("title = ?", "rollback snapshot").Count(&Task{})
	require.NoError(t, err)
	assert.Zero(t, taskCount)
	deliveryCount, err := check.Where("event_name = ?", "task.created").Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	assert.Zero(t, deliveryCount)
}

func TestProjectDeleteRollsBackWhenSnapshotFails(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())

	s := db.NewSession()
	defer s.Close()
	for index := 0; index < 2; index++ {
		webhook := &Webhook{
			TargetURL:   "https://example.com/hook",
			Events:      []string{"project.deleted"},
			ProjectID:   1,
			CreatedByID: 1,
		}
		_, err := s.Insert(webhook)
		require.NoError(t, err)
	}
	events.BindRequestMeta(
		s,
		events.WithRequestMeta(context.Background(), &events.RequestMeta{
			RequestID: "018f8f4c-2e18-7b61-9a50-9c4f5de4e50c",
		}),
	)
	defer events.CleanupPending(s)

	previous := insertDeliverySnapshot
	calls := 0
	insertDeliverySnapshot = func(session *xorm.Session, record *WebhookDeliveryRecord) error {
		calls++
		if calls == 2 {
			return errors.New("injected snapshot failure")
		}
		return previous(session, record)
	}
	t.Cleanup(func() { insertDeliverySnapshot = previous })

	project := &Project{ID: 1}
	err := project.Delete(s, &user.User{ID: 1})
	require.Error(t, err)
	require.NoError(t, s.Rollback())

	check := db.NewSession()
	defer check.Close()
	exists, err := check.ID(int64(1)).Exist(&Project{})
	require.NoError(t, err)
	assert.True(t, exists, "project must survive the rolled back delete")
	deliveryCount, err := check.Where("event_name = ?", "project.deleted").Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	assert.Zero(t, deliveryCount)
}

func TestTaskDeleteRollsBackWhenSnapshotFails(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())

	s := db.NewSession()
	defer s.Close()
	for index := 0; index < 2; index++ {
		webhook := &Webhook{
			TargetURL:   "https://example.com/hook",
			Events:      []string{"task.deleted"},
			ProjectID:   1,
			CreatedByID: 1,
		}
		_, err := s.Insert(webhook)
		require.NoError(t, err)
	}
	events.BindRequestMeta(
		s,
		events.WithRequestMeta(context.Background(), &events.RequestMeta{
			RequestID: "018f8f4c-2e18-7b61-9a50-9c4f5de4e50d",
		}),
	)
	defer events.CleanupPending(s)

	previous := insertDeliverySnapshot
	calls := 0
	insertDeliverySnapshot = func(session *xorm.Session, record *WebhookDeliveryRecord) error {
		calls++
		if calls == 2 {
			return errors.New("injected snapshot failure")
		}
		return previous(session, record)
	}
	t.Cleanup(func() { insertDeliverySnapshot = previous })

	task := &Task{ID: 1}
	err := task.Delete(s, &user.User{ID: 1})
	require.Error(t, err)
	require.NoError(t, s.Rollback())

	check := db.NewSession()
	defer check.Close()
	exists, err := check.ID(int64(1)).Exist(&Task{})
	require.NoError(t, err)
	assert.True(t, exists, "task must survive the rolled back delete")
	deliveryCount, err := check.Where("event_name = ?", "task.deleted").Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	assert.Zero(t, deliveryCount)
}
