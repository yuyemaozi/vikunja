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

package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookDeliveriesJSON(t *testing.T) {
	records := []*models.WebhookDeliveryRecord{
		{
			DeliveryID:   "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b",
			EventName:    "task.updated",
			Status:       "pending",
			AttemptCount: 2,
			CreatedAt:    time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
		},
	}

	payload, err := webhookDeliveriesJSON(records)
	require.NoError(t, err)

	var decoded struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Len(t, decoded.Deliveries, 1)
	item := decoded.Deliveries[0]
	assert.Equal(t, "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b", item["delivery_id"])
	assert.Equal(t, "task.updated", item["event"])
	assert.Equal(t, "pending", item["status"])
	assert.EqualValues(t, 2, item["attempts"])
	assert.Equal(t, "2026-11-04T12:00:00Z", item["created_at"])
}

func TestDeliveryResultJSON(t *testing.T) {
	payload, err := deliveryResultJSON(models.DeliveryResult{
		DeliveryID: "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b",
		Status:     "delivered",
	})
	require.NoError(t, err)

	var decoded struct {
		Summary map[string]int   `json:"summary"`
		Items   []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	assert.Equal(t, 1, decoded.Summary["requested"])
	assert.Equal(t, 1, decoded.Summary["delivered"])
	require.Len(t, decoded.Items, 1)
	assert.Equal(t, "delivered", decoded.Items[0]["status"])
}
