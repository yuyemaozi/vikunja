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
	"os"
	"strings"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A real PG trigger fails the completion UPDATE after the remote accepted it.
// No production fault-injection endpoint or driver shortcut is involved.
func TestDeliveryHTTPAcceptedButDBFinalizeFails(t *testing.T) {
	if os.Getenv("VIKUNJA_DATABASE_TYPE") != "postgres" {
		t.Skip("requires PostgreSQL trigger")
	}
	sourceAcceptanceSetup(t)
	target := newSourceAcceptanceTarget(t)
	record := newPendingDelivery(t, target.url)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	name := "test_webhook_finalize_" + suffix
	s := db.NewSession()
	_, err := s.Exec("CREATE FUNCTION " + name + "() RETURNS trigger AS $$ BEGIN IF NEW.delivery_id = '" + record.DeliveryID + "' AND NEW.status = 'delivered' THEN RAISE EXCEPTION 'synthetic completion failure'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql")
	require.NoError(t, err)
	_, err = s.Exec("CREATE TRIGGER " + name + " BEFORE UPDATE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION " + name + "()")
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	cleanup := func() {
		session := db.NewSession()
		defer session.Close()
		_, dropErr := session.Exec("DROP TRIGGER IF EXISTS " + name + " ON webhook_deliveries")
		require.NoError(t, dropErr)
		_, dropErr = session.Exec("DROP FUNCTION IF EXISTS " + name + "()")
		require.NoError(t, dropErr)
		require.NoError(t, session.Commit())
	}
	t.Cleanup(cleanup)
	result := RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeNormal)
	require.Equal(t, "error", result.Status)
	require.Equal(t, "finalize_failed", result.ErrorCode)
	before := loadDelivery(t, record.DeliveryID)
	require.Equal(t, "processing", before.Status)
	require.Equal(t, 1, before.AttemptCount)
	require.Nil(t, before.DeliveredAt)
	require.Len(t, target.received(), 1)
	cleanup()
	expire := db.NewSession()
	_, err = expire.Exec("UPDATE webhook_deliveries SET lease_until=? WHERE delivery_id=?", time.Now().UTC().Add(-time.Minute), record.DeliveryID)
	require.NoError(t, err)
	require.NoError(t, expire.Commit())
	require.NoError(t, expire.Close())
	require.Equal(t, "delivered", RunWebhookDelivery(context.Background(), record.DeliveryID, DeliveryModeRecover).Status)
	after := loadDelivery(t, record.DeliveryID)
	assert.Equal(t, before.PayloadBody, after.PayloadBody)
	assert.Equal(t, before.PayloadSHA256, after.PayloadSHA256)
	assert.Equal(t, before.RequestID, after.RequestID)
	assert.Equal(t, before.RetryGeneration, after.RetryGeneration)
	assert.Equal(t, 2, after.AttemptCount)
	receipts := target.received()
	require.Len(t, receipts, 2)
	assert.Equal(t, receipts[0], receipts[1])
}
