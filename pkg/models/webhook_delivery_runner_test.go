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
	"os"
	"sync"
	"sync/atomic"
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

func TestDeliveryCASConcurrentNormalAndRecover(t *testing.T) {
	if os.Getenv("VIKUNJA_DATABASE_TYPE") != "postgres" {
		t.Skip("concurrency CAS requires postgres")
	}

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
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := db.NewSession()
			defer session.Close()
			_, result, claimErr := ClaimWebhookDelivery(
				session, record.DeliveryID, DeliveryModeNormal, now, 30*time.Second,
			)
			if claimErr != nil {
				results <- "error"
				return
			}
			if commitErr := session.Commit(); commitErr != nil {
				results <- "error"
				return
			}
			results <- result.Status
		}()
	}
	wg.Wait()
	close(results)

	statuses := map[string]int{}
	for status := range results {
		statuses[status]++
	}
	assert.Equal(t, 1, statuses["processing"])
	assert.Equal(t, 1, statuses["busy"])
}

func TestDeliverySixClaimsAcrossRestart(t *testing.T) {
	allowLoopbackWebhook(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
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

	for attempt := 1; attempt <= 6; attempt++ {
		reset := db.NewSession()
		_, err := reset.Exec(
			"UPDATE webhook_deliveries SET next_attempt_at = ? WHERE delivery_id = ?",
			time.Now().Add(-time.Minute).UTC(), record.DeliveryID,
		)
		require.NoError(t, err)
		require.NoError(t, reset.Commit())
		_ = reset.Close()

		result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
		if attempt < 6 {
			assert.Equal(t, "retry", result.Status, "attempt %d", attempt)
		} else {
			assert.Equal(t, "failed", result.Status)
		}
	}
	assert.EqualValues(t, 6, requests.Load())
	assert.Equal(t, WebhookDeliveryStatusFailed, loadDelivery(t, record.DeliveryID).Status)

	assert.Equal(t, "already_resolved",
		RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal).Status)
	assert.EqualValues(t, 6, requests.Load())

	retried := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeRetry)
	assert.Equal(t, "retry", retried.Status)
	assert.EqualValues(t, 7, requests.Load())
	pending := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, 1, pending.RetryGeneration)
	assert.Equal(t, 1, pending.CycleAttemptCount)
}
