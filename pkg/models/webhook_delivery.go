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
	"fmt"
	"time"

	"code.vikunja.io/api/pkg/db"

	"github.com/google/uuid"
	"xorm.io/xorm"
)

const (
	// WebhookDeliveryStatusPending means the delivery has not been attempted successfully yet.
	WebhookDeliveryStatusPending = "pending"
	// WebhookDeliveryStatusDelivered means the target accepted the payload.
	WebhookDeliveryStatusDelivered = "delivered"
	// WebhookDeliveryStatusFailed means the delivery was poisoned after all retries.
	WebhookDeliveryStatusFailed = "failed"
	// WebhookDeliveryStatusCancelled means the webhook was deleted before delivery.
	WebhookDeliveryStatusCancelled = "cancelled"
	// WebhookDeliveryStatusProcessing marks a delivery claimed by a recovery run.
	WebhookDeliveryStatusProcessing = "processing"
)

// WebhookDeliveryRecord is the durable outbox entry for a single webhook delivery.
// It stores the payload snapshot but never the target URL or credentials.
type WebhookDeliveryRecord struct {
	ID            int64           `xorm:"bigint autoincr not null unique pk" json:"-"`
	DeliveryID    string          `xorm:"varchar(36) not null unique" json:"delivery_id"`
	WebhookID     int64           `xorm:"bigint not null index" json:"webhook_id"`
	EventName     string          `xorm:"varchar(255) not null" json:"event_name"`
	RequestID     string          `xorm:"varchar(36) null" json:"request_id"`
	EventID       string          `xorm:"varchar(36) null" json:"event_id"`
	PayloadBody   []byte          `xorm:"blob null" json:"-"`
	PayloadSHA256 string          `xorm:"'payload_sha256' varchar(64) null" json:"payload_sha256"`
	LeaseToken    string          `xorm:"varchar(36) null" json:"-"`
	LeaseUntil    *time.Time      `xorm:"timestamp null" json:"-"`
	Generation    int             `xorm:"int not null default 0" json:"generation"`
	Payload       *WebhookPayload `xorm:"JSON not null" json:"-"`
	Status        string          `xorm:"varchar(16) not null index" json:"status"`
	AttemptCount  int             `xorm:"int not null default 0" json:"attempt_count"`
	LastErrorCode string          `xorm:"varchar(64) null" json:"last_error_code"`
	CreatedAt     time.Time       `xorm:"created not null" json:"created_at"`
	LastAttemptAt *time.Time      `xorm:"null" json:"last_attempt_at"`
	DeliveredAt   *time.Time      `xorm:"null" json:"delivered_at"`
	FailedAt      *time.Time      `xorm:"null" json:"failed_at"`
}

// TableName returns the table name of the outbox.
func (*WebhookDeliveryRecord) TableName() string {
	return "webhook_deliveries"
}

// CreateWebhookDelivery persists one immutable payload snapshot for a webhook.
func CreateWebhookDelivery(s *xorm.Session, webhookID int64, payload *WebhookPayload, requestIDs ...string) (*WebhookDeliveryRecord, error) {
	requestID := ""
	if len(requestIDs) > 0 {
		requestID = requestIDs[0]
	}
	record, _, err := CreateOrGetWebhookDelivery(
		s,
		uuid.NewString(),
		webhookID,
		payload,
		requestID,
	)
	return record, err
}

// CreateOrGetWebhookDelivery persists one delivery under a stable id. A retried
// fan-out reuses the same id and must not create a second row.
func CreateOrGetWebhookDelivery(
	s *xorm.Session,
	deliveryID string,
	webhookID int64,
	payload *WebhookPayload,
	requestID string,
) (*WebhookDeliveryRecord, bool, error) {
	existing := &WebhookDeliveryRecord{}
	has, err := s.Where("delivery_id = ?", deliveryID).Get(existing)
	if err != nil {
		return nil, false, err
	}
	if has {
		return existing, false, nil
	}

	record := &WebhookDeliveryRecord{
		DeliveryID: deliveryID,
		WebhookID:  webhookID,
		EventName:  payload.EventName,
		RequestID:  requestID,
		Payload:    payload,
		Status:     WebhookDeliveryStatusPending,
	}
	if _, err := s.Insert(record); err != nil {
		// A concurrent fan-out may have inserted the same stable id first.
		has, getErr := s.Where("delivery_id = ?", deliveryID).Get(existing)
		if getErr == nil && has {
			return existing, false, nil
		}
		return nil, false, err
	}
	return record, true, nil
}

// AttemptWebhookDelivery performs one delivery attempt from the persisted snapshot.
func AttemptWebhookDelivery(deliveryID string) error {
	s := db.NewAutocommitSession()
	defer s.Close()

	record := &WebhookDeliveryRecord{}
	has, err := s.Where("delivery_id = ?", deliveryID).Get(record)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("webhook delivery %s does not exist", deliveryID)
	}
	if record.Status == WebhookDeliveryStatusDelivered ||
		record.Status == WebhookDeliveryStatusCancelled ||
		record.Status == WebhookDeliveryStatusFailed {
		return nil
	}
	if record.Payload == nil {
		return fmt.Errorf("webhook delivery %s has no payload", deliveryID)
	}

	webhook := &Webhook{}
	has, err = s.ID(record.WebhookID).Get(webhook)
	if err != nil {
		return err
	}

	now := time.Now()
	if !has {
		record.Status = WebhookDeliveryStatusCancelled
		if _, err := s.ID(record.ID).Cols("status").Update(record); err != nil {
			return err
		}
		return nil
	}

	record.AttemptCount++
	record.LastAttemptAt = &now
	if _, err := s.ID(record.ID).
		Cols("attempt_count", "last_attempt_at").
		Update(record); err != nil {
		return err
	}

	if err := webhook.sendWebhookPayloadWithRequestID(record.Payload, record.RequestID); err != nil {
		record.Status = WebhookDeliveryStatusPending
		record.LastErrorCode = "delivery_failed"
		if _, updateErr := s.ID(record.ID).
			Cols("status", "last_error_code").
			Update(record); updateErr != nil {
			return updateErr
		}
		return err
	}

	record.Status = WebhookDeliveryStatusDelivered
	record.DeliveredAt = &now
	record.LastErrorCode = ""
	if _, err := s.ID(record.ID).
		Cols("status", "delivered_at", "last_error_code").
		Update(record); err != nil {
		return err
	}
	return nil
}

// MarkWebhookDeliveryPoisoned marks one delivery failed after all retries.
func MarkWebhookDeliveryPoisoned(deliveryID string) error {
	s := db.NewAutocommitSession()
	defer s.Close()

	now := time.Now()
	_, err := s.Where(
		"delivery_id = ? AND status NOT IN (?, ?)",
		deliveryID,
		WebhookDeliveryStatusDelivered,
		WebhookDeliveryStatusCancelled,
	).
		Cols("status", "failed_at", "last_error_code").
		Update(&WebhookDeliveryRecord{
			Status:        WebhookDeliveryStatusFailed,
			FailedAt:      &now,
			LastErrorCode: "poisoned",
		})
	return err
}

// ClaimWebhookDeliveryForRecovery atomically claims a pending or failed row.
func ClaimWebhookDeliveryForRecovery(s *xorm.Session, deliveryID string) (bool, error) {
	result, err := s.Exec(
		"UPDATE webhook_deliveries SET status = ? WHERE delivery_id = ? AND status IN (?, ?)",
		WebhookDeliveryStatusProcessing,
		deliveryID,
		WebhookDeliveryStatusPending,
		WebhookDeliveryStatusFailed,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// RecoverableWebhookDeliveries lists pending rows older than the cutoff.
func RecoverableWebhookDeliveries(
	s *xorm.Session,
	olderThan time.Time,
) ([]*WebhookDeliveryRecord, error) {
	records := make([]*WebhookDeliveryRecord, 0)
	err := s.Where("status = ? AND created_at < ?", WebhookDeliveryStatusPending, olderThan).
		OrderBy("created_at ASC, id ASC").
		Find(&records)
	return records, err
}

// RetryWebhookDelivery atomically moves one failed delivery back to pending.
func RetryWebhookDelivery(s *xorm.Session, deliveryID string) (bool, error) {
	result, err := s.Exec(
		"UPDATE webhook_deliveries SET status = ?, failed_at = NULL, last_error_code = '' "+
			"WHERE delivery_id = ? AND status = ?",
		WebhookDeliveryStatusPending,
		deliveryID,
		WebhookDeliveryStatusFailed,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// ListWebhookDeliveries lists outbox entries in the given statuses.
func ListWebhookDeliveries(s *xorm.Session, statuses []string) ([]*WebhookDeliveryRecord, error) {
	if len(statuses) == 0 {
		statuses = []string{WebhookDeliveryStatusPending, WebhookDeliveryStatusFailed}
	}
	records := make([]*WebhookDeliveryRecord, 0)
	err := s.In("status", statuses).
		OrderBy("created_at ASC, id ASC").
		Find(&records)
	return records, err
}
