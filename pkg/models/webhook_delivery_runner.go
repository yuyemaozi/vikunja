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
	"time"

	"code.vikunja.io/api/pkg/db"

	"github.com/google/uuid"
	"xorm.io/xorm"
)

// DeliveryMode selects which persisted rows a claim may pick up.
type DeliveryMode string

const (
	DeliveryModeNormal  DeliveryMode = "normal"
	DeliveryModeRecover DeliveryMode = "recover"
	DeliveryModeRetry   DeliveryMode = "retry"
)

// DeliveryResult is the stable outcome of one driver invocation.
type DeliveryResult struct {
	DeliveryID string
	Status     string
	ErrorCode  string
}

// Lease fences one owner of a processing row.
type Lease struct {
	Token      string
	Generation int
	Until      time.Time
	Attempt    int
}

// DeliveryOutcome is the terminal or retry result of one HTTP attempt.
type DeliveryOutcome struct {
	Kind      string
	ErrorCode string
	RetryAt   *time.Time
}

const (
	OutcomeDelivered = "delivered"
	OutcomeCancelled = "cancelled"
	OutcomeRetry     = "retry"
	OutcomeFailed    = "failed"
)

// ClaimWebhookDelivery atomically takes ownership of one eligible row.
func ClaimWebhookDelivery(
	s *xorm.Session,
	id string,
	mode DeliveryMode,
	now time.Time,
	ttl time.Duration,
) (Lease, DeliveryResult, error) {
	token := uuid.NewString()
	until := now.Add(ttl)

	where := ""
	switch mode {
	case DeliveryModeNormal, DeliveryModeRecover:
		where = "(status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)) " +
			"OR (status = 'processing' AND lease_until <= ?)"
	case DeliveryModeRetry:
		where = "status = 'failed'"
	default:
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "invalid_mode"},
			fmt.Errorf("unsupported delivery mode %q", mode)
	}

	generation := "retry_generation"
	attempts := "cycle_attempt_count + 1"
	if mode == DeliveryModeRetry {
		generation = "retry_generation + 1"
		attempts = "1"
	}

	result, err := s.Exec(
		"UPDATE webhook_deliveries SET status = 'processing', lease_token = ?, lease_until = ?, "+
			"cycle_attempt_count = "+attempts+", retry_generation = "+generation+" "+
			"WHERE delivery_id = ? AND ("+where+")",
		token, until, id, now, now,
	)
	if err != nil {
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}, err
	}
	if affected == 0 {
		record := &WebhookDeliveryRecord{}
		has, err := s.Where("delivery_id = ?", id).Get(record)
		if err != nil {
			return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "read_failed"}, err
		}
		if !has {
			return Lease{}, DeliveryResult{DeliveryID: id, Status: "not_found"}, nil
		}
		if record.Status == WebhookDeliveryStatusDelivered ||
			record.Status == WebhookDeliveryStatusCancelled ||
			record.Status == WebhookDeliveryStatusFailed {
			return Lease{}, DeliveryResult{DeliveryID: id, Status: "already_resolved"}, nil
		}
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "busy"}, nil
	}

	record := &WebhookDeliveryRecord{}
	has, err := s.Where("delivery_id = ?", id).Get(record)
	if err != nil || !has {
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "read_failed"}, err
	}
	return Lease{
		Token:      token,
		Generation: record.RetryGeneration,
		Until:      until,
		Attempt:    record.CycleAttemptCount,
	}, DeliveryResult{DeliveryID: id, Status: "processing"}, nil
}

// CompleteWebhookDelivery writes one outcome only if this lease still owns the row.
func CompleteWebhookDelivery(
	s *xorm.Session,
	id string,
	lease Lease,
	now time.Time,
	outcome DeliveryOutcome,
) (bool, error) {
	status := ""
	nextAttempt := any(nil)
	switch outcome.Kind {
	case OutcomeDelivered:
		status = WebhookDeliveryStatusDelivered
	case OutcomeCancelled:
		status = WebhookDeliveryStatusCancelled
	case OutcomeRetry:
		status = WebhookDeliveryStatusPending
		nextAttempt = outcome.RetryAt
	case OutcomeFailed:
		status = WebhookDeliveryStatusFailed
	default:
		return false, fmt.Errorf("unsupported delivery outcome %q", outcome.Kind)
	}

	result, err := s.Exec(
		"UPDATE webhook_deliveries SET status = ?, last_error_code = ?, lease_token = NULL, "+
			"lease_until = NULL, next_attempt_at = ? "+
			"WHERE delivery_id = ? AND status = 'processing' AND lease_token = ? "+
			"AND retry_generation = ? AND lease_until > ?",
		status, outcome.ErrorCode, nextAttempt, id, lease.Token, lease.Generation, now,
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

var deliveryNow = func() time.Time { return time.Now().UTC() }

var deliveryBackoff = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1600 * time.Millisecond,
}

const (
	deliveryLeaseTTL   = 30 * time.Second
	deliveryMaxAttempt = 6
)

var deliverySleeper = func(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RunWebhookDelivery runs one bounded retry cycle: claim, send outside the
// transaction, persist the outcome, wait for the persisted retry time and
// re-claim. It never holds a transaction while sending or sleeping.
func RunWebhookDelivery(ctx context.Context, id string, mode DeliveryMode) DeliveryResult {
	deadline := deliveryNow().Add(6*deliveryLeaseTTL + 10*time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	claimMode := mode

	for {
		if ctx.Err() != nil {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "cancelled"}
		}
		now := deliveryNow()
		if now.After(deadline) {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "deadline"}
		}

		session := db.NewSession()
		lease, result, err := ClaimWebhookDelivery(session, id, claimMode, now, deliveryLeaseTTL)
		if err != nil {
			_ = session.Close()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}
		}
		if result.Status != "processing" {
			_ = session.Close()
			return result
		}

		record := &WebhookDeliveryRecord{}
		has, err := session.Where("delivery_id = ?", id).Get(record)
		webhook := &Webhook{}
		webhookFound := false
		if has && err == nil {
			webhookFound, err = session.Where("id = ?", record.WebhookID).Get(webhook)
		}
		if commitErr := session.Commit(); commitErr != nil {
			err = commitErr
		}
		_ = session.Close()
		if err != nil || !has {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "read_failed"}
		}

		outcome := DeliveryOutcome{Kind: OutcomeDelivered}
		if !webhookFound {
			outcome = DeliveryOutcome{Kind: OutcomeCancelled, ErrorCode: "target_deleted"}
		} else if sendErr := webhook.sendWebhookPayloadWithRequestID(record.Payload, record.RequestID); sendErr != nil {
			if lease.Attempt >= deliveryMaxAttempt {
				outcome = DeliveryOutcome{Kind: OutcomeFailed, ErrorCode: "delivery_failed"}
			} else {
				backoff := deliveryBackoff[min(lease.Attempt-1, len(deliveryBackoff)-1)]
				retryAt := deliveryNow().Add(backoff)
				outcome = DeliveryOutcome{
					Kind:      OutcomeRetry,
					ErrorCode: "delivery_failed",
					RetryAt:   &retryAt,
				}
			}
		}

		completeSession := db.NewSession()
		completed, err := CompleteWebhookDelivery(completeSession, id, lease, deliveryNow(), outcome)
		if err != nil || !completed {
			_ = completeSession.Close()
			return DeliveryResult{DeliveryID: id, Status: "busy", ErrorCode: "lease_lost"}
		}
		if commitErr := completeSession.Commit(); commitErr != nil {
			_ = completeSession.Close()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "finalize_failed"}
		}
		_ = completeSession.Close()

		switch outcome.Kind {
		case OutcomeDelivered, OutcomeCancelled, OutcomeFailed:
			return DeliveryResult{DeliveryID: id, Status: outcome.Kind, ErrorCode: outcome.ErrorCode}
		}

		wait := outcome.RetryAt.Sub(deliveryNow())
		if err := deliverySleeper(ctx, wait); err != nil {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "cancelled"}
		}
		claimMode = DeliveryModeNormal
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
