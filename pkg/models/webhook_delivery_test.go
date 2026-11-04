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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestWebhook(t *testing.T, targetURL string) *Webhook {
	t.Helper()
	s := db.NewSession()
	defer s.Close()
	webhook := &Webhook{
		TargetURL:   targetURL,
		Events:      []string{"task.updated"},
		ProjectID:   1,
		CreatedByID: 1,
	}
	_, err := s.Insert(webhook)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	return webhook
}

func loadDelivery(t *testing.T, deliveryID string) *WebhookDeliveryRecord {
	t.Helper()
	s := db.NewSession()
	defer s.Close()
	record := &WebhookDeliveryRecord{}
	has, err := s.Where("delivery_id = ?", deliveryID).Get(record)
	require.NoError(t, err)
	require.True(t, has)
	return record
}

func allowLoopbackWebhook(t *testing.T) {
	t.Helper()
	previousAllow := config.OutgoingRequestsAllowNonRoutableIPs.GetBool()
	previousClient := webhookClient
	config.OutgoingRequestsAllowNonRoutableIPs.Set(true)
	webhookClient = nil
	t.Cleanup(func() {
		config.OutgoingRequestsAllowNonRoutableIPs.Set(previousAllow)
		webhookClient = previousClient
	})
}

func TestCreateWebhookDeliveryPersistsSnapshot(t *testing.T) {
	webhook := newTestWebhook(t, "https://example.com/hook")
	payload := &WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{"task": map[string]interface{}{"id": float64(1)}},
	}

	s := db.NewSession()
	defer s.Close()
	record, err := CreateWebhookDelivery(s, webhook.ID, payload)
	require.NoError(t, err)
	require.NoError(t, s.Commit())

	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, webhook.ID, stored.WebhookID)
	assert.Equal(t, "task.updated", stored.EventName)
	assert.Equal(t, WebhookDeliveryStatusPending, stored.Status)
	assert.Equal(t, 0, stored.AttemptCount)
	require.NotNil(t, stored.Payload)
	assert.Equal(t, "task.updated", stored.Payload.EventName)
}

func TestAttemptWebhookDeliveryDeliversAndMarksRecord(t *testing.T) {
	allowLoopbackWebhook(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhook := newTestWebhook(t, server.URL)
	s := db.NewSession()
	record, err := CreateWebhookDelivery(s, webhook.ID, &WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{},
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()

	require.NoError(t, AttemptWebhookDelivery(record.DeliveryID))

	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusDelivered, stored.Status)
	assert.Equal(t, 1, stored.AttemptCount)
	assert.NotNil(t, stored.DeliveredAt)
	assert.NotNil(t, stored.LastAttemptAt)
}

func TestAttemptWebhookDeliveryFailureKeepsRecordPending(t *testing.T) {
	allowLoopbackWebhook(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	webhook := newTestWebhook(t, server.URL)
	s := db.NewSession()
	record, err := CreateWebhookDelivery(s, webhook.ID, &WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{},
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()

	require.Error(t, AttemptWebhookDelivery(record.DeliveryID))

	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusPending, stored.Status)
	assert.Equal(t, 1, stored.AttemptCount)
	assert.Equal(t, "delivery_failed", stored.LastErrorCode)
}

func TestAttemptWebhookDeliveryCancelsDeletedWebhook(t *testing.T) {
	s := db.NewSession()
	record, err := CreateWebhookDelivery(s, 9_999_999, &WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{},
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()

	require.NoError(t, AttemptWebhookDelivery(record.DeliveryID))

	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusCancelled, stored.Status)
}

func TestMarkWebhookDeliveryPoisonedAndRetry(t *testing.T) {
	webhook := newTestWebhook(t, "https://example.com/hook")
	s := db.NewSession()
	record, err := CreateWebhookDelivery(s, webhook.ID, &WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{},
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()

	require.NoError(t, MarkWebhookDeliveryPoisoned(record.DeliveryID))
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusFailed, stored.Status)
	assert.NotNil(t, stored.FailedAt)
	assert.Equal(t, "poisoned", stored.LastErrorCode)

	s = db.NewAutocommitSession()
	defer s.Close()
	retried, err := RetryWebhookDelivery(s, record.DeliveryID)
	require.NoError(t, err)
	assert.True(t, retried)

	retriedAgain, err := RetryWebhookDelivery(s, record.DeliveryID)
	require.NoError(t, err)
	assert.False(t, retriedAgain)
}
