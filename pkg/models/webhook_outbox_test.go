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

func TestOutboxRecursiveProjectDeletionKeepsInheritedSubscriptions(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())
	s := db.NewSession()
	defer s.Close()
	events.BindRequestMeta(s, context.Background())
	defer events.CleanupPending(s)
	owner := &user.User{ID: 1}
	parent := &Project{Title: "recursive parent"}
	require.NoError(t, parent.Create(s, owner))
	child := &Project{Title: "recursive child", ParentProjectID: &parent.ID}
	require.NoError(t, child.Create(s, owner))
	_, err := s.Insert(&ProjectUser{ProjectID: parent.ID, UserID: 2, Permission: PermissionWrite})
	require.NoError(t, err)
	allowed, _, err := child.CanRead(s, &user.User{ID: 2})
	require.NoError(t, err)
	require.True(t, allowed)
	hook := &Webhook{ProjectID: parent.ID, CreatedByID: 2, Events: []string{"project.deleted"}, TargetURL: "https://example.invalid/hook"}
	_, err = s.Insert(hook)
	require.NoError(t, err)
	require.NoError(t, parent.Delete(s, owner))
	records := []*WebhookDeliveryRecord{}
	require.NoError(t, s.Where("webhook_id=? AND event_name=?", hook.ID, "project.deleted").Find(&records))
	require.Len(t, records, 2, "parent and child snapshots must exist before inherited ACLs disappear")
	ids := map[int64]bool{}
	for _, record := range records {
		var envelope map[string]interface{}
		require.NoError(t, decodeEventPayload(record.PayloadBody, &envelope))
		data := envelope["data"].(map[string]interface{})
		project := data["project"].(map[string]interface{})
		ids[getIDAsInt64(project["id"])] = true
	}
	assert.True(t, ids[parent.ID])
	assert.True(t, ids[child.ID])
	require.NoError(t, s.Commit())
}

func TestTaskDeleteSnapshotRetainsOriginalBuckets(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())
	s := db.NewSession()
	defer s.Close()
	events.BindRequestMeta(s, context.Background())
	defer events.CleanupPending(s)
	hook := &Webhook{ProjectID: 1, CreatedByID: 1, Events: []string{"task.deleted"}, TargetURL: "https://example.invalid/hook"}
	_, err := s.Insert(hook)
	require.NoError(t, err)
	require.NoError(t, (&Task{ID: 1}).Delete(s, &user.User{ID: 1}))
	record := &WebhookDeliveryRecord{}
	has, err := s.Where("webhook_id=?", hook.ID).Get(record)
	require.NoError(t, err)
	require.True(t, has)
	var envelope map[string]interface{}
	require.NoError(t, decodeEventPayload(record.PayloadBody, &envelope))
	data := envelope["data"].(map[string]interface{})
	task := data["task"].(map[string]interface{})
	buckets, _ := task["buckets"].([]interface{})
	require.Len(t, buckets, 1, "snapshot must retain original view/bucket before deleting task relations")
	assert.EqualValues(t, 1, getIDAsInt64(buckets[0].(map[string]interface{})["id"]))
	remaining, err := s.Where("task_id=?", 1).Count(&TaskBucket{})
	require.NoError(t, err)
	assert.Zero(t, remaining)
}

func TestOutboxSnapshotsUseSubscriptionCreatorContext(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())
	s := db.NewSession()
	defer s.Close()
	events.BindRequestMeta(s, context.Background())
	defer events.CleanupPending(s)
	actor := &user.User{ID: 1}
	creator := &user.User{ID: 2}
	project := &Project{Title: "creator scoped snapshot"}
	require.NoError(t, project.Create(s, actor))
	_, err := s.Insert(&ProjectUser{ProjectID: project.ID, UserID: creator.ID, Permission: PermissionWrite})
	require.NoError(t, err)
	task := &Task{Title: "creator scoped task", ProjectID: project.ID}
	require.NoError(t, task.Create(s, actor))
	require.NoError(t, addToFavorites(s, project.ID, actor, FavoriteKindProject))
	require.NoError(t, addToFavorites(s, task.ID, creator, FavoriteKindTask))
	creatorSubscription := &Subscription{EntityType: SubscriptionEntityProject, EntityID: project.ID, UserID: creator.ID}
	_, err = s.Insert(creatorSubscription)
	require.NoError(t, err)
	hook := &Webhook{ProjectID: project.ID, CreatedByID: creator.ID, TargetURL: "https://example.invalid/hook", Events: []string{(&TaskUpdatedEvent{}).Name(), (&ProjectUpdatedEvent{}).Name(), (&TaskReminderFiredEvent{}).Name()}}
	_, err = s.Insert(hook)
	require.NoError(t, err)
	for _, event := range []events.Event{
		&TaskUpdatedEvent{Task: task, Doer: actor},
		&ProjectUpdatedEvent{Project: &Project{ID: project.ID}, Doer: actor},
		&TaskReminderFiredEvent{Task: task, User: actor, Project: project},
	} {
		require.NoError(t, events.DispatchOnCommit(s, event))
	}
	records := []*WebhookDeliveryRecord{}
	require.NoError(t, s.Where("webhook_id=?", hook.ID).Find(&records))
	require.Len(t, records, 3)
	for _, record := range records {
		var body map[string]interface{}
		require.NoError(t, decodeEventPayload(record.PayloadBody, &body))
		data := body["data"].(map[string]interface{})
		snapshot := data["project"].(map[string]interface{})
		assert.Equal(t, false, snapshot["is_favorite"], record.EventName+" must not contain actor's personal project favorite")
		subscription, ok := snapshot["subscription"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, creatorSubscription.ID, getIDAsInt64(subscription["id"]))
		views, _ := snapshot["views"].([]interface{})
		assert.NotEmpty(t, views, record.EventName+" must include complete project views")
		if taskData, ok := data["task"].(map[string]interface{}); ok {
			assert.Equal(t, true, taskData["is_favorite"], record.EventName+" must use creator's task favorite even without a doer")
		}
	}
}

func TestOutboxAncestorSubscriptionKeepsSourceProject(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())
	s := db.NewSession()
	defer s.Close()
	events.BindRequestMeta(s, context.Background())
	defer events.CleanupPending(s)
	owner := &user.User{ID: 1}
	parent := &Project{Title: "ancestor subscription"}
	require.NoError(t, parent.Create(s, owner))
	child := &Project{Title: "event source", ParentProjectID: &parent.ID}
	require.NoError(t, child.Create(s, owner))
	task := &Task{Title: "child task", ProjectID: child.ID}
	require.NoError(t, task.Create(s, owner))
	hook := &Webhook{ProjectID: parent.ID, CreatedByID: 1, Events: []string{"task.updated"}, TargetURL: "https://example.invalid/hook"}
	_, err := s.Insert(hook)
	require.NoError(t, err)
	require.NoError(t, events.DispatchOnCommit(s, &TaskUpdatedEvent{Task: task, Doer: owner}))
	record := &WebhookDeliveryRecord{}
	has, err := s.Where("webhook_id=?", hook.ID).Get(record)
	require.NoError(t, err)
	require.True(t, has)
	var body map[string]interface{}
	require.NoError(t, decodeEventPayload(record.PayloadBody, &body))
	project := body["data"].(map[string]interface{})["project"].(map[string]interface{})
	assert.Equal(t, child.ID, getIDAsInt64(project["id"]))
}

func TestOutboxSnapshotPreservesLargeTaskID(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())
	s := db.NewSession()
	defer s.Close()
	events.BindRequestMeta(s, context.Background())
	defer events.CleanupPending(s)
	const id int64 = 9007199254740993
	task := &Task{ID: id, Index: 99999, ProjectID: 1, CreatedByID: 1, Title: "large integer fixture", UID: "large-integer-fixture"}
	_, err := s.Insert(task)
	require.NoError(t, err)
	hook := &Webhook{ProjectID: 1, CreatedByID: 1, Events: []string{"task.updated"}, TargetURL: "https://example.invalid/hook"}
	_, err = s.Insert(hook)
	require.NoError(t, err)
	require.NoError(t, events.DispatchOnCommit(s, &TaskUpdatedEvent{Task: task, Doer: &user.User{ID: 1}}))
	record := &WebhookDeliveryRecord{}
	has, err := s.Where("webhook_id=?", hook.ID).Get(record)
	require.NoError(t, err)
	require.True(t, has)
	var body map[string]interface{}
	require.NoError(t, decodeEventPayload(record.PayloadBody, &body))
	data := body["data"].(map[string]interface{})["task"].(map[string]interface{})
	number, ok := data["id"].(json.Number)
	require.True(t, ok)
	got, err := number.Int64()
	require.NoError(t, err)
	assert.Equal(t, id, got)
}

func TestOutboxOverdueBatchFreezesPersonalCreatorContext(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	require.NoError(t, RegisterWebhookOutbox())
	s := db.NewSession()
	defer s.Close()
	events.BindRequestMeta(s, context.Background())
	defer events.CleanupPending(s)
	owner := &user.User{ID: 1}
	favorite, err := isFavorite(s, 1, owner, FavoriteKindTask)
	require.NoError(t, err)
	if !favorite {
		require.NoError(t, addToFavorites(s, 1, owner, FavoriteKindTask))
	}
	personal := &Webhook{UserID: 1, CreatedByID: 1, Events: []string{"tasks.overdue"}, TargetURL: "https://example.invalid/hook"}
	_, err = s.Insert(personal)
	require.NoError(t, err)
	projectHook := &Webhook{ProjectID: 1, CreatedByID: 1, Events: []string{"tasks.overdue"}, TargetURL: "https://example.invalid/hook"}
	_, err = s.Insert(projectHook)
	require.NoError(t, err)
	require.NoError(t, events.DispatchOnCommit(s, &TasksOverdueEvent{User: owner, Tasks: []*Task{{ID: 1, ProjectID: 1}}, Projects: map[int64]*Project{1: {ID: 1}}}))
	records := []*WebhookDeliveryRecord{}
	require.NoError(t, s.In("webhook_id", []int64{personal.ID, projectHook.ID}).Find(&records))
	require.Len(t, records, 1, "a multi-project user-directed batch matches only the recipient's personal subscription")
	assert.Equal(t, personal.ID, records[0].WebhookID)
	var body map[string]interface{}
	require.NoError(t, decodeEventPayload(records[0].PayloadBody, &body))
	data := body["data"].(map[string]interface{})
	tasks := data["tasks"].([]interface{})
	require.Len(t, tasks, 1)
	assert.Equal(t, true, tasks[0].(map[string]interface{})["is_favorite"])
	projects := data["projects"].(map[string]interface{})
	views, _ := projects["1"].(map[string]interface{})["views"].([]interface{})
	assert.NotEmpty(t, views)
}
