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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm/schemas"
)

func requireDeliveryPG16(t *testing.T) {
	t.Helper()
	if db.Type() != schemas.POSTGRES {
		t.Skip("real concurrency and SQL cancellation require PostgreSQL 16")
	}
	s := db.NewReadSession()
	defer s.Close()
	db.SetSessionContext(t.Context(), s)
	rows, err := s.QueryString("SELECT current_setting('server_version_num')::integer / 10000 AS major")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "16", rows[0]["major"], "acceptance runner must use locked PostgreSQL 16")
}

// Both transactions reach the start gate before either attempts the same row.
// Errors and Commit results cross the channel instead of using Fatal in workers.
func concurrentDeliveryClaims(t *testing.T, record *WebhookDeliveryRecord, modes [2]DeliveryMode) {
	t.Helper()
	type claimOutcome struct {
		status string
		token  string
		err    error
	}
	results := make(chan claimOutcome, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, mode := range modes {
		go func(mode DeliveryMode) {
			s := db.NewAutocommitSession()
			defer s.Close()
			db.SetSessionContext(t.Context(), s)
			err := s.Begin()
			ready.Done()
			<-start
			if err != nil {
				results <- claimOutcome{err: err}
				return
			}
			now, err := deliveryDatabaseNow(s)
			if err != nil {
				results <- claimOutcome{err: err}
				return
			}
			lease, result, err := ClaimWebhookDelivery(s, record.DeliveryID, mode, now, time.Minute)
			if err == nil {
				err = s.Commit()
			}
			results <- claimOutcome{status: result.Status, token: lease.Token, err: err}
		}(mode)
	}
	ready.Wait()
	close(start)
	statuses := map[string]int{}
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		statuses[result.status]++
		if result.status == "processing" {
			assert.NotEmpty(t, result.token)
		} else {
			assert.Empty(t, result.token)
		}
	}
	assert.Equal(t, map[string]int{"processing": 1, "busy": 1}, statuses)
}

func TestDeliveryAcceptanceConcurrentRetryStartsOneGeneration(t *testing.T) {
	requireDeliveryPG16(t)
	db.LoadAndAssertFixtures(t)
	record := newPendingDelivery(t, "https://example.invalid/hook")
	s := db.NewSession()
	_, err := s.Where("delivery_id = ?", record.DeliveryID).Cols("status", "retry_generation", "cycle_attempt_count", "attempt_count").Update(&WebhookDeliveryRecord{
		Status: WebhookDeliveryStatusFailed, RetryGeneration: 4, CycleAttemptCount: 6, AttemptCount: 12,
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	concurrentDeliveryClaims(t, record, [2]DeliveryMode{DeliveryModeRetry, DeliveryModeRetry})
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusProcessing, stored.Status)
	assert.Equal(t, 5, stored.RetryGeneration)
	assert.Equal(t, 1, stored.CycleAttemptCount)
	assert.Equal(t, 13, stored.AttemptCount)
}

func TestDeliveryAcceptanceConcurrentNormalAndRecover(t *testing.T) {
	requireDeliveryPG16(t)
	db.LoadAndAssertFixtures(t)
	record := newPendingDelivery(t, "https://example.invalid/hook")
	concurrentDeliveryClaims(t, record, [2]DeliveryMode{DeliveryModeNormal, DeliveryModeRecover})
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, 0, stored.RetryGeneration)
	assert.Equal(t, 1, stored.CycleAttemptCount)
	assert.Equal(t, 1, stored.AttemptCount)
}

func TestDeliveryAcceptanceBudgetSurvivesNewDriver(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	record := newPendingDelivery(t, server.URL)
	// Each of the first three persisted attempts releases every session before
	// the next phase, as if a former driver had exited after its committed retry.
	for attempt := 1; attempt <= 3; attempt++ {
		claim := db.NewSession()
		db.SetSessionContext(t.Context(), claim)
		now, err := deliveryDatabaseNow(claim)
		require.NoError(t, err)
		lease, result, err := ClaimWebhookDelivery(claim, record.DeliveryID, DeliveryModeNormal, now, time.Minute)
		require.NoError(t, err)
		require.Equal(t, "processing", result.Status)
		require.Equal(t, attempt, lease.Attempt)
		require.NoError(t, claim.Commit())
		require.NoError(t, claim.Close())

		require.Error(t, sendPersistedWebhook(t.Context(), &Webhook{TargetURL: server.URL}, loadDelivery(t, record.DeliveryID)))

		completion := db.NewSession()
		db.SetSessionContext(t.Context(), completion)
		now, err = deliveryDatabaseNow(completion)
		require.NoError(t, err)
		completed, err := CompleteWebhookDelivery(completion, record.DeliveryID, lease, now, DeliveryOutcome{Kind: OutcomeRetry, ErrorCode: "delivery_failed", RetryAt: &now})
		require.NoError(t, err)
		require.True(t, completed)
		require.NoError(t, completion.Commit())
		require.NoError(t, completion.Close())
	}
	require.EqualValues(t, 3, requests.Load())
	resumed := RunWebhookDelivery(t.Context(), record.DeliveryID, DeliveryModeRecover)
	assert.Equal(t, "failed", resumed.Status)
	assert.EqualValues(t, 6, requests.Load(), "a new invocation has only three attempts left")
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, WebhookDeliveryStatusFailed, stored.Status)
	assert.Equal(t, 6, stored.CycleAttemptCount)
	assert.Equal(t, 6, stored.AttemptCount)
	assert.Zero(t, stored.RetryGeneration)
	again := RunWebhookDelivery(t.Context(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "already_resolved", again.Status)
	assert.EqualValues(t, 6, requests.Load(), "normal delivery must never send a seventh request")
}

func TestDeliveryAcceptanceLegacySnapshotFrozenAcrossRetry(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	type wireRequest struct {
		body      []byte
		requestID string
		err       error
	}
	requests := make(chan wireRequest, 8)
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		requests <- wireRequest{body: body, requestID: r.Header.Get("X-Request-ID"), err: err}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	webhook := newTestWebhook(t, server.URL)
	original := &WebhookPayload{EventName: "task.updated", Time: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), Data: map[string]interface{}{"legacy": "original"}}
	expectedBody, err := json.Marshal(original)
	require.NoError(t, err)
	record := &WebhookDeliveryRecord{DeliveryID: uuid.NewString(), WebhookID: webhook.ID, EventName: "task.updated", Payload: original, RequestID: "invalid-legacy-id", Status: WebhookDeliveryStatusPending}
	s := db.NewSession()
	_, err = s.Insert(record)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	before := loadDelivery(t, record.DeliveryID)
	require.Empty(t, before.PayloadBody)
	require.Empty(t, before.PayloadSHA256)
	result := RunWebhookDelivery(t.Context(), record.DeliveryID, DeliveryModeNormal)
	require.Equal(t, "failed", result.Status)
	frozen := loadDelivery(t, record.DeliveryID)
	require.Equal(t, expectedBody, frozen.PayloadBody)
	hash := sha256.Sum256(expectedBody)
	require.Equal(t, hex.EncodeToString(hash[:]), frozen.PayloadSHA256)
	_, err = uuid.Parse(frozen.RequestID)
	require.NoError(t, err)
	require.NotEqual(t, "invalid-legacy-id", frozen.RequestID)
	// Alter only the obsolete JSON field; the frozen wire snapshot must win.
	s = db.NewSession()
	_, err = s.Where("delivery_id = ?", record.DeliveryID).Cols("payload").Update(&WebhookDeliveryRecord{Payload: &WebhookPayload{EventName: "task.updated", Data: map[string]interface{}{"legacy": "changed"}}})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	fail.Store(false)
	retried := RunWebhookDelivery(t.Context(), record.DeliveryID, DeliveryModeRetry)
	require.Equal(t, "delivered", retried.Status)
	after := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, frozen.PayloadBody, after.PayloadBody)
	assert.Equal(t, frozen.PayloadSHA256, after.PayloadSHA256)
	assert.Equal(t, frozen.RequestID, after.RequestID)
	assert.Equal(t, 1, after.RetryGeneration)
	assert.Equal(t, 7, after.AttemptCount)
	require.Len(t, requests, 7)
	for range 7 {
		request := <-requests
		require.NoError(t, request.err)
		assert.Equal(t, expectedBody, request.body)
		assert.Equal(t, frozen.RequestID, request.requestID)
	}
}

func TestDeliveryAcceptanceDatabasePhaseDeadline(t *testing.T) {
	requireDeliveryPG16(t)
	db.LoadAndAssertFixtures(t)
	for _, test := range []struct {
		name          string
		callerTimeout time.Duration
		minimum       time.Duration
		maximum       time.Duration
	}{
		{name: "phase limit", callerTimeout: 15 * time.Second, minimum: 4 * time.Second, maximum: 7 * time.Second},
		{name: "earlier caller deadline", callerTimeout: 250 * time.Millisecond, minimum: 100 * time.Millisecond, maximum: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancelCaller := context.WithTimeout(t.Context(), test.callerTimeout)
			defer cancelCaller()
			started := time.Now()
			s, cancel, err := newDeliveryPhase(ctx)
			require.NoError(t, err)
			defer cancel()
			defer s.Close()
			_, err = s.QueryString("SELECT pg_sleep(8)")
			elapsed := time.Since(started)
			require.Error(t, err, "the SQL query must be cancelled by the real session context")
			assert.GreaterOrEqual(t, elapsed, test.minimum)
			assert.Less(t, elapsed, test.maximum)
		})
	}
}
