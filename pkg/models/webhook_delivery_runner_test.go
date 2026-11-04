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
