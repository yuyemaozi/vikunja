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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/mail"
	"code.vikunja.io/api/pkg/notifications"
	"code.vikunja.io/api/pkg/user"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
	"xorm.io/xorm/names"
)

type sourceAcceptanceReceipt struct {
	body      []byte
	requestID string
}

type sourceAcceptanceTarget struct {
	url      string
	mu       sync.Mutex
	receipts []sourceAcceptanceReceipt
}

func newSourceAcceptanceTarget(t *testing.T) *sourceAcceptanceTarget {
	t.Helper()
	target := &sourceAcceptanceTarget{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		target.mu.Lock()
		target.receipts = append(target.receipts, sourceAcceptanceReceipt{body: body, requestID: r.Header.Get("X-Request-ID")})
		target.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	target.url = server.URL
	t.Cleanup(server.Close)
	return target
}

func (target *sourceAcceptanceTarget) received() []sourceAcceptanceReceipt {
	target.mu.Lock()
	defer target.mu.Unlock()
	return append([]sourceAcceptanceReceipt(nil), target.receipts...)
}

func sourceAcceptanceSetup(t *testing.T) {
	t.Helper()
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	require.NoError(t, RegisterWebhookOutbox())
	events.Fake()
}

// Clone only connection configuration, never the global engine or its lifetime.
// Using the effective test DSN also preserves the PG runner's isolated database,
// search_path and SSL options, and SQLite's actual shared-memory/file test path.
func sourceAcceptanceIndependentEngine(t *testing.T) *xorm.Engine {
	t.Helper()
	engine, err := xorm.NewEngine(x.DriverName(), x.DataSourceName())
	require.NoError(t, err)
	require.NotSame(t, x, engine)
	engine.SetMapper(names.GonicMapper{})
	engine.SetTZLocation(config.GetTimeZone())
	if x.DriverName() == "pgx" {
		engine.SetSchema(config.DatabaseSchema.GetString())
	}
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	return engine
}

func sourceAcceptanceStageTask(t *testing.T, target string) (*Task, []WebhookDeliveryRecord, string) {
	t.Helper()
	requestID := uuid.NewString()
	s := db.NewSession()
	defer s.Close()
	defer events.CleanupPending(s)
	events.BindRequestMeta(s, events.WithRequestMeta(context.Background(), &events.RequestMeta{RequestID: requestID}))
	owner := &user.User{ID: 1}
	project := &Project{Title: "source acceptance " + requestID}
	require.NoError(t, project.Create(s, owner))
	for range 2 {
		hook := &Webhook{ProjectID: project.ID, CreatedByID: owner.ID, Events: []string{"task.created"}, TargetURL: target}
		_, err := s.Insert(hook)
		require.NoError(t, err)
	}
	task := &Task{ProjectID: project.ID, Title: "original source " + requestID}
	require.NoError(t, task.Create(s, owner))
	var rows []WebhookDeliveryRecord
	require.NoError(t, s.Where("request_id = ? AND event_name = ?", requestID, "task.created").OrderBy("webhook_id").Find(&rows))
	require.Len(t, rows, 2)
	require.Equal(t, rows[0].EventID, rows[1].EventID)
	require.NoError(t, s.Commit())
	// Deliberately omit DispatchPending: the real business transaction and every
	// snapshot are committed, then the memory queue is discarded as on process exit.
	events.CleanupPending(s)
	return task, rows, requestID
}

func TestOutboxSourceCommitWithoutWakeupRecoversFromIndependentEngine(t *testing.T) {
	sourceAcceptanceSetup(t)
	target := newSourceAcceptanceTarget(t)
	task, before, requestID := sourceAcceptanceStageTask(t, target.url)
	require.Empty(t, target.received())
	require.Empty(t, events.GetDispatchedEvents((&TaskCreatedEvent{}).Name()))
	require.Empty(t, events.GetDispatchedEvents((&WebhookDeliveryEvent{}).Name()))

	engine := sourceAcceptanceIndependentEngine(t)
	read := engine.NewSession()
	var savedTask Task
	exists, err := read.ID(task.ID).Get(&savedTask)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, task.Title, savedTask.Title)
	var stored []WebhookDeliveryRecord
	require.NoError(t, read.Where("event_id = ?", before[0].EventID).OrderBy("webhook_id").Find(&stored))
	require.Len(t, stored, 2)
	for index, row := range stored {
		require.Equal(t, WebhookDeliveryStatusPending, row.Status)
		require.Zero(t, row.AttemptCount)
		require.Equal(t, requestID, row.RequestID)
		require.Equal(t, before[index].PayloadBody, row.PayloadBody)
		require.Equal(t, before[index].PayloadSHA256, row.PayloadSHA256)
	}
	require.NoError(t, read.Close())

	for _, row := range stored {
		result := RunWebhookDelivery(context.Background(), row.DeliveryID, DeliveryModeRecover)
		require.Equal(t, "delivered", result.Status, "recovery error code: %s", result.ErrorCode)
	}
	receipts := target.received()
	require.Len(t, receipts, 2)
	for index, receipt := range receipts {
		require.Equal(t, stored[index].PayloadBody, receipt.body)
		require.Equal(t, requestID, receipt.requestID)
	}
	check := engine.NewSession()
	defer check.Close()
	var delivered []WebhookDeliveryRecord
	require.NoError(t, check.Where("event_id = ?", before[0].EventID).Find(&delivered))
	require.Len(t, delivered, 2)
	for _, row := range delivered {
		require.Equal(t, WebhookDeliveryStatusDelivered, row.Status)
		require.Equal(t, 1, row.AttemptCount)
	}
}

func TestOutboxSourceSnapshotSurvivesTaskUpdateAndLaterSubscription(t *testing.T) {
	sourceAcceptanceSetup(t)
	target := newSourceAcceptanceTarget(t)
	task, before, requestID := sourceAcceptanceStageTask(t, target.url)

	edit := db.NewSession()
	defer edit.Close()
	defer events.CleanupPending(edit)
	events.BindRequestMeta(edit, context.Background())
	changed := &Task{ID: task.ID}
	require.NoError(t, changed.ReadOne(edit, &user.User{ID: 1}))
	changed.Title = "later task mutation"
	require.NoError(t, changed.Update(edit, &user.User{ID: 1}))
	hook := &Webhook{ProjectID: task.ProjectID, CreatedByID: 1, Events: []string{"task.created"}, TargetURL: target.url}
	_, err := edit.Insert(hook)
	require.NoError(t, err)
	require.NoError(t, edit.Commit())
	events.CleanupPending(edit)

	engine := sourceAcceptanceIndependentEngine(t)
	read := engine.NewSession()
	var unchanged []WebhookDeliveryRecord
	require.NoError(t, read.Where("event_id = ?", before[0].EventID).OrderBy("webhook_id").Find(&unchanged))
	require.Len(t, unchanged, 2, "the new subscription must not fan out the old event")
	for index, row := range unchanged {
		require.Equal(t, before[index].DeliveryID, row.DeliveryID)
		require.Equal(t, before[index].PayloadBody, row.PayloadBody)
		require.Equal(t, before[index].PayloadSHA256, row.PayloadSHA256)
		require.Equal(t, requestID, row.RequestID)
	}
	require.NoError(t, read.Close())
	for _, row := range unchanged {
		require.Equal(t, "delivered", RunWebhookDelivery(context.Background(), row.DeliveryID, DeliveryModeRecover).Status)
	}
	require.Len(t, target.received(), 2)
	for index, receipt := range target.received() {
		require.Equal(t, before[index].PayloadBody, receipt.body)
	}
}

// Covers the actual function used inside RegisterReminderCron. It does not run
// a wall-clock scheduler or substitute a hand-written reminder producer.
func TestOutboxSourceReminderSecondSnapshotFailureRollsBackNotification(t *testing.T) {
	sourceAcceptanceSetup(t)
	enableReminderMail(t)
	notifications.Unfake()
	t.Cleanup(notifications.Unfake)
	target := newSourceAcceptanceTarget(t)
	setup := db.NewSession()
	for range 2 {
		_, err := setup.Insert(&Webhook{UserID: 1, CreatedByID: 1, Events: []string{"task.reminder.fired"}, TargetURL: target.url})
		require.NoError(t, err)
	}
	require.NoError(t, setup.Commit())
	require.NoError(t, setup.Close())
	require.Empty(t, mail.SentMails())

	s := db.NewSession()
	defer s.Close()
	defer events.CleanupPending(s)
	requestID := uuid.NewString()
	events.BindRequestMeta(s, events.WithRequestMeta(context.Background(), &events.RequestMeta{RequestID: requestID}))
	beforeNotifications, err := s.Count(&notifications.DatabaseNotification{})
	require.NoError(t, err)
	oldInsert := insertDeliverySnapshot
	sentinel := errors.New("source acceptance second reminder snapshot failure")
	calls := 0
	insertDeliverySnapshot = func(session *xorm.Session, row *WebhookDeliveryRecord) error {
		if row.EventName == "task.reminder.fired" {
			calls++
			if calls == 2 {
				return sentinel
			}
		}
		return oldInsert(session, row)
	}
	t.Cleanup(func() { insertDeliverySnapshot = oldInsert })
	err = sendDueReminders(s, time.Date(2018, 12, 1, 1, 12, 0, 0, time.UTC), true)
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 2, calls)
	require.NoError(t, s.Rollback())
	events.CleanupPending(s)
	require.Empty(t, mail.SentMails(), "rolled-back reminder must not queue mail")
	require.Empty(t, target.received())
	require.Empty(t, events.GetDispatchedEvents((&TaskReminderFiredEvent{}).Name()))
	require.Empty(t, events.GetDispatchedEvents((&WebhookDeliveryEvent{}).Name()))
	check := db.NewSession()
	defer check.Close()
	afterNotifications, err := check.Count(&notifications.DatabaseNotification{})
	require.NoError(t, err)
	require.Equal(t, beforeNotifications, afterNotifications)
	count, err := check.Where("request_id = ?", requestID).Count(&WebhookDeliveryRecord{})
	require.NoError(t, err)
	require.Zero(t, count)
}

// Both overdue source types run the actual transaction body registered by cron.
// The injected second-snapshot error must roll back rows and discard mail.
func TestOutboxSourceOverdueCronSecondSnapshotFailureDoesNotSendMail(t *testing.T) {
	for _, eventName := range []string{"task.overdue", "tasks.overdue"} {
		t.Run(eventName, func(t *testing.T) {
			sourceAcceptanceSetup(t)
			enableReminderMail(t)
			notifications.Unfake()
			t.Cleanup(notifications.Unfake)
			target := newSourceAcceptanceTarget(t)
			setup := db.NewSession()
			_, err := setup.ID(1).Cols("timezone", "overdue_tasks_reminders_time", "overdue_tasks_reminders_enabled").Update(&user.User{
				Timezone: "UTC", OverdueTasksRemindersTime: "09:00", OverdueTasksRemindersEnabled: true,
			})
			require.NoError(t, err)
			for range 2 {
				_, err = setup.Insert(&Webhook{UserID: 1, CreatedByID: 1, Events: []string{eventName}, TargetURL: target.url})
				require.NoError(t, err)
			}
			require.NoError(t, setup.Commit())
			require.NoError(t, setup.Close())
			require.Empty(t, mail.SentMails())

			s := db.NewSession()
			defer s.Close()
			defer events.CleanupPending(s)
			requestID := uuid.NewString()
			events.BindRequestMeta(s, events.WithRequestMeta(context.Background(), &events.RequestMeta{RequestID: requestID}))
			beforeNotifications, err := s.Count(&notifications.DatabaseNotification{})
			require.NoError(t, err)
			oldInsert := insertDeliverySnapshot
			calls := 0
			sentinel := errors.New("source acceptance second overdue snapshot failure")
			insertDeliverySnapshot = func(session *xorm.Session, row *WebhookDeliveryRecord) error {
				if row.EventName == eventName {
					calls++
					if calls == 2 {
						return sentinel
					}
				}
				return oldInsert(session, row)
			}
			t.Cleanup(func() { insertDeliverySnapshot = oldInsert })
			err = sendOverdueReminders(s, time.Date(2018, 12, 1, 9, 0, 0, 0, time.UTC), true, true)
			require.ErrorIs(t, err, sentinel)
			require.Equal(t, 2, calls)
			require.NoError(t, s.Rollback())
			events.CleanupPending(s)
			require.Empty(t, mail.SentMails(), "snapshot staging failure must not leak an overdue email")
			require.Empty(t, target.received())
			require.Empty(t, events.GetDispatchedEvents(eventName))
			require.Empty(t, events.GetDispatchedEvents((&WebhookDeliveryEvent{}).Name()))
			check := db.NewSession()
			defer check.Close()
			count, err := check.Where("request_id = ?", requestID).Count(&WebhookDeliveryRecord{})
			require.NoError(t, err)
			require.Zero(t, count)
			afterNotifications, err := check.Count(&notifications.DatabaseNotification{})
			require.NoError(t, err)
			require.Equal(t, beforeNotifications, afterNotifications)
		})
	}
}

func TestOutboxSourceOverdueMailWaitsForCommitAndDispatch(t *testing.T) {
	sourceAcceptanceSetup(t)
	enableReminderMail(t)
	notifications.Unfake()
	t.Cleanup(notifications.Unfake)
	s := db.NewSession()
	defer s.Close()
	defer events.CleanupPending(s)
	events.BindRequestMeta(s, context.Background())
	_, err := s.ID(1).Cols("timezone", "overdue_tasks_reminders_time", "overdue_tasks_reminders_enabled").Update(&user.User{
		Timezone: "UTC", OverdueTasksRemindersTime: "09:00", OverdueTasksRemindersEnabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, sendOverdueReminders(s, time.Date(2018, 12, 1, 9, 0, 0, 0, time.UTC), true, false))
	require.Empty(t, mail.SentMails(), "mail must be deferred while the transaction is still open")
	require.NoError(t, s.Commit())
	events.DispatchPending(context.Background(), s)
	require.Len(t, mail.SentMails(), 1)
}
