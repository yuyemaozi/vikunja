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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryTransitions(t *testing.T) {
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

	s = db.NewSession()
	now := time.Now().UTC()
	lease, result, err := ClaimWebhookDelivery(s, record.DeliveryID, DeliveryModeNormal, now, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, "processing", result.Status)
	require.NotEmpty(t, lease.Token)
	require.NoError(t, s.Commit())
	_ = s.Close()

	s = db.NewSession()
	completed, err := CompleteWebhookDelivery(s, record.DeliveryID, lease, now, DeliveryOutcome{
		Kind: OutcomeDelivered,
	})
	require.NoError(t, err)
	assert.True(t, completed)
	require.NoError(t, s.Commit())
	_ = s.Close()

	assert.Equal(t, WebhookDeliveryStatusDelivered, loadDelivery(t, record.DeliveryID).Status)

	s = db.NewSession()
	defer s.Close()
	_, again, err := ClaimWebhookDelivery(s, record.DeliveryID, DeliveryModeNormal, now, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "already_resolved", again.Status)

	completed, err = CompleteWebhookDelivery(s, record.DeliveryID, lease, now, DeliveryOutcome{
		Kind: OutcomeDelivered,
	})
	require.NoError(t, err)
	assert.False(t, completed)
}

func TestRunWebhookDeliveryDelivered(t *testing.T) {
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

	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)

	assert.Equal(t, "delivered", result.Status)
	assert.Equal(t, WebhookDeliveryStatusDelivered, loadDelivery(t, record.DeliveryID).Status)
}

func TestRunWebhookDeliveryRetriesThenFails(t *testing.T) {
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

	first := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "retry", first.Status)
	pending := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusPending, pending.Status)
	assert.NotNil(t, pending.NextAttemptAt)

	_, err = s.ID(record.ID).Cols("cycle_attempt_count").Update(&WebhookDeliveryRecord{CycleAttemptCount: 5})
	require.NoError(t, err)

	sixth := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "failed", sixth.Status)
	assert.Equal(t, WebhookDeliveryStatusFailed, loadDelivery(t, record.DeliveryID).Status)
	_ = s.Close()
}

func TestDeliveryExpiredOwnerCannotFinalize(t *testing.T) {
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

	now := time.Now().UTC()
	s = db.NewSession()
	leaseA, result, err := ClaimWebhookDelivery(s, record.DeliveryID, DeliveryModeNormal, now, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, "processing", result.Status)
	require.NoError(t, s.Commit())
	_ = s.Close()

	later := now.Add(31 * time.Second)
	s = db.NewSession()
	leaseB, result, err := ClaimWebhookDelivery(s, record.DeliveryID, DeliveryModeNormal, later, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, "processing", result.Status)
	require.NotEqual(t, leaseA.Token, leaseB.Token)
	require.NoError(t, s.Commit())
	_ = s.Close()

	s = db.NewSession()
	stale, err := CompleteWebhookDelivery(s, record.DeliveryID, leaseA, later, DeliveryOutcome{
		Kind: OutcomeDelivered,
	})
	require.NoError(t, err)
	assert.False(t, stale)
	require.NoError(t, s.Commit())
	_ = s.Close()

	s = db.NewSession()
	completed, err := CompleteWebhookDelivery(s, record.DeliveryID, leaseB, later, DeliveryOutcome{
		Kind: OutcomeDelivered,
	})
	require.NoError(t, err)
	assert.True(t, completed)
	require.NoError(t, s.Commit())
	_ = s.Close()

	assert.Equal(t, WebhookDeliveryStatusDelivered, loadDelivery(t, record.DeliveryID).Status)
}
