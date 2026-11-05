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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryPermissionRevokedAfterSnapshotCancels(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	record := newPendingDelivery(t, server.URL)
	s := db.NewSession()
	_, err := s.Exec("UPDATE webhooks SET project_id = 3 WHERE id = ?", record.WebhookID)
	require.NoError(t, err)
	_, err = s.Exec("DELETE FROM users_projects WHERE project_id = 3 AND user_id = 1")
	require.NoError(t, err)
	_, err = s.Exec("DELETE FROM team_projects WHERE project_id = 3")
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "cancelled", result.Status)
	assert.Zero(t, requests.Load())
	assert.Equal(t, "cancelled", loadDelivery(t, record.DeliveryID).Status)
}

func TestDeliveryDisabledBeforeSendCancels(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	record := newPendingDelivery(t, server.URL)
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(false)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "cancelled", result.Status)
	assert.Zero(t, requests.Load())
}

func TestDeliveryPermissionLookupFailureIsRecoverable(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := db.NewReadSession()
	db.SetSessionContext(ctx, s)
	defer s.Close()
	reason, err := deliveryAuthorization(s, &Webhook{CreatedByID: 1, ProjectID: 1, Events: []string{"task.updated"}}, &WebhookDeliveryRecord{EventName: "task.updated"})
	require.Error(t, err)
	assert.Empty(t, reason, "a database failure must not be reported as proven revocation")
}

func TestDeliveryLogHasCorrelationAndRedactsContent(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	var output bytes.Buffer
	log.SetOutputOverride(&output)
	log.ConfigureStandardLogger(true, "stdout", ".", "INFO", "structured")
	t.Cleanup(func() { log.SetOutputOverride(nil); log.InitLogger() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("private-response-marker"))
	}))
	defer server.Close()
	record := newPendingDelivery(t, server.URL+"/?secret=private-url-marker")
	require.Equal(t, "failed", RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal).Status)
	stored := loadDelivery(t, record.DeliveryID)
	found := false
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var event map[string]interface{}
		require.NoError(t, decoder.Decode(&event))
		if event["event"] != "webhook.delivery.attempt_completed" {
			continue
		}
		found = true
		assert.Equal(t, "vikunja", event["service"])
		assert.Equal(t, stored.RequestID, event["request_id"])
		assert.Equal(t, record.DeliveryID, event["delivery_id"])
		assert.NotEmpty(t, event["timestamp"])
		assert.Contains(t, event, "attempt")
		assert.Contains(t, event, "generation")
		assert.Contains(t, event, "duration_ms")
	}
	assert.True(t, found, "critical delivery outcome must be structured and correlated")
	assert.NotContains(t, output.String(), "private-response-marker")
	assert.NotContains(t, output.String(), "private-url-marker")
}

func TestDeliveryDisabledBotOwnerCancels(t *testing.T) {
	for _, status := range []user.Status{user.StatusDisabled, user.StatusAccountLocked, user.StatusEmailConfirmationRequired} {
		t.Run(status.String(), func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			allowLoopbackWebhook(t)
			var requests atomic.Int32
			webhookClient = &http.Client{Transport: deliveryRoundTrip(func(_ *http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
			})}
			record := newPendingDelivery(t, "http://127.0.0.1/hook")
			s := db.NewSession()
			_, err := s.Exec("UPDATE users SET bot_owner_id=2 WHERE id=1")
			require.NoError(t, err)
			_, err = s.Exec("UPDATE users SET status=? WHERE id=2", status)
			require.NoError(t, err)
			require.NoError(t, s.Commit())
			require.NoError(t, s.Close())
			assert.Equal(t, "cancelled", RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal).Status)
			assert.Zero(t, requests.Load())
		})
	}
}
