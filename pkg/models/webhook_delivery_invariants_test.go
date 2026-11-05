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
	"sync/atomic"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
)

func TestDeliveryDatabaseClockSkewDoesNotCancelHTTP(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	previous := deliveryDatabaseNow
	deliveryDatabaseNow = func(_ *xorm.Session) (time.Time, error) { return time.Now().UTC().Add(-time.Hour), nil }
	t.Cleanup(func() { deliveryDatabaseNow = previous })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	record := newPendingDelivery(t, server.URL)
	assert.Equal(t, "delivered", RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal).Status)
	assert.EqualValues(t, 1, calls.Load())
}

func TestDeliveryCorruptPayloadFailsWithoutHTTP(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	record := newPendingDelivery(t, "http://127.0.0.1/hook")
	s := db.NewSession()
	_, err := s.Exec("UPDATE webhook_deliveries SET payload_body=? WHERE delivery_id=?", []byte("{"), record.DeliveryID)
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	assert.Equal(t, "failed", result.Status)
	assert.Equal(t, "payload_corrupt", result.ErrorCode)
	stored := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, "failed", stored.Status)
	assert.Equal(t, 1, stored.CycleAttemptCount)
}

func TestDeliveryExpiredOwnerCannotMaterializeLegacy(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	record := newPendingDelivery(t, "http://127.0.0.1/hook")
	s := db.NewSession()
	defer s.Close()
	now := time.Now().UTC()
	lease, _, err := ClaimWebhookDelivery(s, record.DeliveryID, DeliveryModeNormal, now, time.Minute)
	require.NoError(t, err)
	previous := deliveryDatabaseNow
	deliveryDatabaseNow = func(_ *xorm.Session) (time.Time, error) { return now.Add(2 * time.Minute), nil }
	t.Cleanup(func() { deliveryDatabaseNow = previous })
	require.Error(t, materializeDelivery(s, record, lease))
	// Check the transaction itself: an invalid owner must not write even before rollback.
	stored := &WebhookDeliveryRecord{}
	has, err := s.ID(record.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Empty(t, stored.PayloadBody)
	assert.Empty(t, stored.RequestID)
}

func TestDeliveryNon2xxNeverDelivered(t *testing.T) {
	db.LoadAndAssertFixtures(t)
	allowLoopbackWebhook(t)
	withFakeDeliveryClock(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(304) }))
	defer server.Close()
	record := newPendingDelivery(t, server.URL)
	assert.Equal(t, "failed", RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal).Status)
	assert.EqualValues(t, 6, calls.Load())
}
