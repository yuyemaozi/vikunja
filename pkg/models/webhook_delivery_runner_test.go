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
	"io"
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

func withFakeDeliveryClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now().UTC()
	previousNow := deliveryNow
	previousSleeper := deliverySleeper
	deliveryNow = func() time.Time { return now }
	deliverySleeper = func(ctx context.Context, duration time.Duration) error {
		now = now.Add(duration)
		return ctx.Err()
	}
	t.Cleanup(func() {
		deliveryNow = previousNow
		deliverySleeper = previousSleeper
	})
	return &now
}

func newPendingDelivery(t *testing.T, targetURL string) *WebhookDeliveryRecord {
	t.Helper()
	webhook := newTestWebhook(t, targetURL)
	s := db.NewSession()
	record, err := CreateWebhookDelivery(s, webhook.ID, &WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{},
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()
	return record
}

func TestDeliveryDriverRetriesWithoutAnotherWakeup(t *testing.T) {
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	record := newPendingDelivery(t, server.URL)
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)

	assert.Equal(t, "delivered", result.Status)
	assert.EqualValues(t, 2, requests.Load())
	assert.Equal(t, WebhookDeliveryStatusDelivered, loadDelivery(t, record.DeliveryID).Status)
}

func TestDeliveryDriverExhaustsSixAttempts(t *testing.T) {
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	record := newPendingDelivery(t, server.URL)
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)

	assert.Equal(t, "failed", result.Status)
	assert.EqualValues(t, 6, requests.Load())
	assert.Equal(t, WebhookDeliveryStatusFailed, loadDelivery(t, record.DeliveryID).Status)

	again := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "already_resolved", again.Status)
	assert.EqualValues(t, 6, requests.Load())
}

func TestDeliveryDriverCancelDuringBackoff(t *testing.T) {
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	record := newPendingDelivery(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	previousSleeper := deliverySleeper
	deliverySleeper = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	t.Cleanup(func() { deliverySleeper = previousSleeper })

	result := RunWebhookDelivery(ctx, record.DeliveryID, DeliveryModeNormal)

	assert.Equal(t, "error", result.Status)
	assert.Equal(t, "cancelled", result.ErrorCode)
	persisted := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusPending, persisted.Status)
	assert.NotNil(t, persisted.NextAttemptAt)
}

func TestDeliveryDriverDeadlineBoundsAttempts(t *testing.T) {
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	record := newPendingDelivery(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	previousSleeper := deliverySleeper
	deliverySleeper = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return context.Canceled
	}
	t.Cleanup(func() { deliverySleeper = previousSleeper })

	result := RunWebhookDelivery(ctx, record.DeliveryID, DeliveryModeNormal)

	assert.NotEqual(t, "delivered", result.Status)
	assert.NotEqual(t, "failed", result.Status)
}

func TestDeliveryNetworkDoesNotHoldDBTransaction(t *testing.T) {
	allowLoopbackWebhook(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	record := newPendingDelivery(t, server.URL)
	resultCh := make(chan DeliveryResult, 1)
	go func() {
		resultCh <- RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("driver never reached the HTTP send")
	}

	queries := make(chan int, 1)
	go func() {
		s := db.NewSession()
		defer s.Close()
		count, err := s.Where("delivery_id = ?", record.DeliveryID).Count(&WebhookDeliveryRecord{})
		if err != nil {
			queries <- -1
			return
		}
		queries <- int(count)
	}()

	select {
	case count := <-queries:
		assert.Equal(t, 1, count)
	case <-time.After(2 * time.Second):
		t.Fatal("database was locked while the HTTP request was in flight")
	}

	close(release)
	select {
	case result := <-resultCh:
		assert.Equal(t, "delivered", result.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("driver did not finish")
	}
}

func TestDeliverySendsPersistedBytes(t *testing.T) {
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	body := []byte(`{"event_name":"task.updated","time":"2026-11-04T00:00:00Z","data":{"n":12345678901234567890}}`)
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhook := newTestWebhook(t, server.URL)
	s := db.NewSession()
	record := &WebhookDeliveryRecord{
		DeliveryID:  "018f8f4c-2e18-7b61-9a50-9c4f5de4e301",
		WebhookID:   webhook.ID,
		EventName:   "task.updated",
		Payload:     &WebhookPayload{EventName: "task.updated", Data: map[string]interface{}{"n": 1}},
		PayloadBody: body,
		Status:      WebhookDeliveryStatusPending,
	}
	_, err := s.Insert(record)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()

	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "delivered", result.Status)
	assert.Equal(t, body, received)
}

func TestDeliveryHTTPHonorsCallerDeadline(t *testing.T) {
	allowLoopbackWebhook(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	record := newPendingDelivery(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := RunWebhookDelivery(ctx, record.DeliveryID, DeliveryModeNormal)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 3*time.Second, "HTTP must use the caller context")
	assert.NotEqual(t, "delivered", result.Status)
	assert.Equal(t, WebhookDeliveryStatusPending, loadDelivery(t, record.DeliveryID).Status)
}

func TestLegacyAttemptCannotStealValidLease(t *testing.T) {
	allowLoopbackWebhook(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
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

	now := time.Now().UTC()
	s = db.NewSession()
	lease, result, err := ClaimWebhookDelivery(s, record.DeliveryID, DeliveryModeNormal, now, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, "processing", result.Status)
	require.NoError(t, s.Commit())
	_ = s.Close()

	require.NoError(t, AttemptWebhookDelivery(record.DeliveryID))
	assert.Zero(t, requests.Load(), "a valid lease must block the legacy attempt")

	persisted := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusProcessing, persisted.Status)
	assert.Equal(t, lease.Token, persisted.LeaseToken)
}
