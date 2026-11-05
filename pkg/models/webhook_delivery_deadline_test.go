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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xorm.io/xorm"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type deliveryRoundTrip func(*http.Request) (*http.Response, error)

func (f deliveryRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeliveryHTTPHasInternalDeadline(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	old := config.WebhooksTimeoutSeconds.GetInt()
	config.WebhooksTimeoutSeconds.Set(1)
	t.Cleanup(func() { config.WebhooksTimeoutSeconds.Set(old) })
	record := newPendingDelivery(t, "http://127.0.0.1/hook")
	webhookClient = &http.Client{Transport: deliveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		assert.True(t, ok, "HTTP must have a driver/lease bounded context even without caller deadline")
		if ok {
			assert.LessOrEqual(t, time.Until(deadline), 16*time.Second)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	assert.Equal(t, "delivered", RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal).Status)
}

func TestDeliverySixthClaimCrashBecomesFailed(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	record := newPendingDelivery(t, "http://127.0.0.1/hook")
	s := db.NewSession()
	_, err := s.Exec("UPDATE webhook_deliveries SET status='processing', cycle_attempt_count=6, lease_token='expired',lease_until=? WHERE delivery_id=?", time.Now().Add(-time.Minute), record.DeliveryID)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeRecover)
	assert.Equal(t, "failed", result.Status)
	assert.Equal(t, "failed", loadDelivery(t, record.DeliveryID).Status)
}

func TestDeliveryCancellationStopsInFlightHTTP(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	record := newPendingDelivery(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan DeliveryResult, 1)
	go func() { results <- RunWebhookDelivery(ctx, record.DeliveryID, DeliveryModeNormal) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP context was not cancelled")
	}
	select {
	case result := <-results:
		assert.Equal(t, "error", result.Status)
	case <-time.After(6 * time.Second):
		t.Fatal("driver did not stop")
	}
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusPending, stored.Status)
	assert.Equal(t, 1, stored.CycleAttemptCount)
}

func TestDeliveryDatabaseClockFailureDoesNotSend(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	record := newPendingDelivery(t, "http://127.0.0.1/hook")
	previous := deliveryDatabaseNow
	deliveryDatabaseNow = func(_ *xorm.Session) (time.Time, error) { return time.Time{}, fmt.Errorf("clock unavailable") }
	t.Cleanup(func() { deliveryDatabaseNow = previous })
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "error", result.Status)
	assert.Zero(t, loadDelivery(t, record.DeliveryID).AttemptCount)
}

func TestDeliveryRetryPendingKeepsGeneration(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	webhookClient = &http.Client{Transport: deliveryRoundTrip(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	record := newPendingDelivery(t, "http://127.0.0.1/hook")
	s := db.NewSession()
	_, err := s.Exec("UPDATE webhook_deliveries SET retry_generation=4, cycle_attempt_count=2 WHERE delivery_id=?", record.DeliveryID)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeRetry)
	assert.Equal(t, "delivered", result.Status)
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, 4, stored.RetryGeneration)
	assert.Equal(t, 3, stored.CycleAttemptCount)
}
