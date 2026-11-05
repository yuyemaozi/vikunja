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
	"log/slog"
	"time"

	"code.vikunja.io/api/pkg/log"
	"github.com/google/uuid"
)

type deliveryObservation struct {
	id, requestID, origin string
	generation, attempt   int
	started               time.Time
}

func newDeliveryObservation(id string) *deliveryObservation {
	return &deliveryObservation{id: id, requestID: uuid.NewString(), origin: "operation", started: time.Now()}
}

func (o *deliveryObservation) bind(record *WebhookDeliveryRecord, lease Lease) {
	o.generation, o.attempt = lease.Generation, lease.Attempt
	if record == nil {
		return
	}
	if id, err := uuid.Parse(record.RequestID); err == nil {
		o.requestID = id.String()
		o.origin = "delivery"
	}
}

func (o *deliveryObservation) finish(ctx context.Context, result *DeliveryResult) {
	level := slog.LevelInfo
	if result.Status == "error" || result.Status == "failed" {
		level = slog.LevelError
	}
	log.Event(ctx, level, "webhook.delivery.result", "request_id", o.requestID, "request_id_origin", o.origin, "delivery_id", o.id, "generation", o.generation, "attempt", o.attempt, "status", result.Status, "error_code", result.ErrorCode, "duration_ms", time.Since(o.started).Milliseconds())
}
