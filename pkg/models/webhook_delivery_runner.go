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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/log"

	"github.com/google/uuid"
	"xorm.io/xorm"
	"xorm.io/xorm/schemas"
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
	Token        string
	Generation   int
	Until        time.Time
	Attempt      int
	databaseTime time.Time
	wallStarted  time.Time
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
	wallStarted := time.Now()
	if mode != DeliveryModeNormal && mode != DeliveryModeRecover && mode != DeliveryModeRetry {
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "invalid_mode"}, fmt.Errorf("unsupported delivery mode")
	}

	{
		// An exhausted lease can be finalized without starting a seventh attempt.
		result, err := s.Exec("UPDATE webhook_deliveries SET status = 'failed', failed_at = ?, lease_token = NULL, lease_until = NULL, next_attempt_at = NULL, last_error_code = 'attempts_exhausted' WHERE delivery_id = ? AND cycle_attempt_count >= 6 AND ((status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)) OR (status = 'processing' AND lease_until <= ?))", now, id, now, now)
		if err != nil {
			return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}, err
		}
		if affected > 0 {
			return Lease{}, DeliveryResult{DeliveryID: id, Status: "failed", ErrorCode: "attempts_exhausted"}, nil
		}
	}
	token := uuid.NewString()
	until := now.Add(ttl)

	where := ""
	switch mode {
	case DeliveryModeNormal, DeliveryModeRecover:
		where = "(status = 'pending' AND cycle_attempt_count < 6 " +
			"AND (next_attempt_at IS NULL OR next_attempt_at <= ?)) " +
			"OR (status = 'processing' AND cycle_attempt_count < 6 AND lease_until <= ?)"
	case DeliveryModeRetry:
		where = "status = 'failed' OR (status = 'pending' AND cycle_attempt_count < 6 AND (next_attempt_at IS NULL OR next_attempt_at <= ?)) OR (status = 'processing' AND cycle_attempt_count < 6 AND lease_until <= ?)"
	default:
		return Lease{}, DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "invalid_mode"},
			fmt.Errorf("unsupported delivery mode %q", mode)
	}

	generation := "retry_generation"
	attempts := "cycle_attempt_count + 1"
	if mode == DeliveryModeRetry {
		generation = "CASE WHEN status = 'failed' THEN retry_generation + 1 ELSE retry_generation END"
		attempts = "CASE WHEN status = 'failed' THEN 1 ELSE cycle_attempt_count + 1 END"
	}

	arguments := []any{token, until, now, id}
	arguments = append(arguments, now, now)
	statement := "UPDATE webhook_deliveries SET lease_token = ?, lease_until = ?, " +
		"attempt_count = attempt_count + 1, last_attempt_at = ?, " +
		"cycle_attempt_count = " + attempts + ", retry_generation = " + generation + ", failed_at = CASE WHEN status = 'failed' THEN NULL ELSE failed_at END, status = 'processing' " +
		"WHERE delivery_id = ? AND (" + where + ")"
	result, err := s.Exec(append([]any{statement}, arguments...)...)
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
		Token:        token,
		Generation:   record.RetryGeneration,
		Until:        until,
		Attempt:      record.CycleAttemptCount,
		databaseTime: now,
		wallStarted:  wallStarted,
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
	var deliveredAt any
	switch outcome.Kind {
	case OutcomeDelivered:
		status = WebhookDeliveryStatusDelivered
		deliveredAt = now
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
			"lease_until = NULL, next_attempt_at = ?, last_attempt_at = ?, "+
			"delivered_at = COALESCE(?, delivered_at) "+
			"WHERE delivery_id = ? AND status = 'processing' AND lease_token = ? "+
			"AND retry_generation = ? AND lease_until > ?",
		status, outcome.ErrorCode, nextAttempt, now, deliveredAt, id, lease.Token, lease.Generation, now,
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

var errDeliveryPayloadCorrupt = errors.New("stored webhook payload is corrupt")

// The database owns eligibility and lease time; the wall clock only bounds work.
var deliveryDatabaseNow = func(s *xorm.Session) (time.Time, error) {
	query := "SELECT UTC_TIMESTAMP(6) AS current_time"
	switch db.Type() { //nolint:exhaustive // Vikunja supports only SQLite, PostgreSQL and MySQL.
	case schemas.SQLITE:
		query = "SELECT strftime('%Y-%m-%d %H:%M:%f', 'now') AS current_time"
	case schemas.POSTGRES:
		query = "SELECT clock_timestamp() AS current_time"
	}
	rows, err := s.QueryString(query)
	if err != nil {
		return time.Time{}, err
	}
	if len(rows) != 1 {
		return time.Time{}, fmt.Errorf("database clock unavailable")
	}
	value := rows[0]["current_time"]
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999"} {
		if now, err := time.Parse(layout, value); err == nil {
			return now.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid database clock")
}

var deliveryBackoff = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1600 * time.Millisecond,
}

const (
	deliveryLeaseTTL     = 30 * time.Second
	deliveryMaxAttempt   = 6
	deliveryDBPhaseLimit = 5 * time.Second
)

func newDeliveryPhase(ctx context.Context) (*xorm.Session, context.CancelFunc, error) {
	phaseCtx, cancel := context.WithTimeout(ctx, deliveryDBPhaseLimit)
	session := db.NewAutocommitSession()
	db.SetSessionContext(phaseCtx, session)
	if err := session.Begin(); err != nil {
		cancel()
		_ = session.Close()
		return nil, cancel, err
	}
	return session, cancel, nil
}

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
func RunWebhookDelivery(ctx context.Context, id string, mode DeliveryMode) (result DeliveryResult) {
	observation := newDeliveryObservation(id)
	defer observation.finish(ctx, &result)

	httpTimeout := time.Duration(config.WebhooksTimeoutSeconds.GetInt()) * time.Second
	if httpTimeout <= 0 {
		return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "invalid_timeout"}
	}
	leaseTTL := httpTimeout + 30*time.Second
	ctx, cancelDriver := context.WithTimeout(ctx, 6*httpTimeout+10*time.Second)
	defer cancelDriver()
	deadline, _ := ctx.Deadline()
	claimMode := mode

	for {
		attemptStarted := time.Now()
		if ctx.Err() != nil {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "cancelled"}
		}
		now := time.Now().UTC()
		if now.After(deadline) {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "deadline"}
		}

		session, cancel, err := newDeliveryPhase(ctx)
		if err != nil {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}
		}
		now, err = deliveryDatabaseNow(session)
		if err != nil {
			cancel()
			_ = session.Close()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "clock_failed"}
		}
		lease, result, err := ClaimWebhookDelivery(session, id, claimMode, now, leaseTTL)
		if err != nil {
			cancel()
			_ = session.Close()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}
		}
		if result.Status != "processing" {
			if err := session.Commit(); err != nil {
				cancel()
				_ = session.Close()
				return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "claim_failed"}
			}
			cancel()
			_ = session.Close()
			return result
		}

		record, webhook, cancelReason, err := prepareClaimedDelivery(session, id, lease)
		observation.bind(record, lease)

		if commitErr := session.Commit(); commitErr != nil {
			err = commitErr
		}
		cancel()
		_ = session.Close()
		if err != nil && !errors.Is(err, errDeliveryPayloadCorrupt) {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "read_failed"}
		}
		var outcome DeliveryOutcome
		if errors.Is(err, errDeliveryPayloadCorrupt) {
			outcome = DeliveryOutcome{Kind: OutcomeFailed, ErrorCode: "payload_corrupt"}
		} else {
			outcome = attemptWebhookDelivery(ctx, webhook, record, lease, cancelReason)
		}

		cleanupCtx, cancelCleanup := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		completeSession, cancelComplete, phaseErr := newDeliveryPhase(cleanupCtx)
		cancelCleanupAfterPhase := func() { cancelComplete(); cancelCleanup() }
		if phaseErr != nil {
			cancelCleanup()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "finalize_failed"}
		}
		completedAt, clockErr := deliveryDatabaseNow(completeSession)
		if clockErr != nil {
			cancelCleanupAfterPhase()
			_ = completeSession.Close()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "clock_failed"}
		}
		if outcome.Kind == OutcomeRetry {
			retryAt := completedAt.Add(deliveryBackoff[min(lease.Attempt-1, len(deliveryBackoff)-1)])
			outcome.RetryAt = &retryAt
		}
		completed, err := CompleteWebhookDelivery(completeSession, id, lease, completedAt, outcome)
		if err != nil || !completed {
			cancelCleanupAfterPhase()
			_ = completeSession.Close()
			if err != nil {
				return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "finalize_failed"}
			}
			log.Event(ctx, slog.LevelWarn, "webhook.delivery.lease_lost", "request_id", record.RequestID, "delivery_id", id, "generation", lease.Generation, "attempt", lease.Attempt)
			return DeliveryResult{DeliveryID: id, Status: "busy", ErrorCode: "lease_lost"}
		}
		if commitErr := completeSession.Commit(); commitErr != nil {
			cancelCleanupAfterPhase()
			_ = completeSession.Close()
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "finalize_failed"}
		}
		cancelCleanupAfterPhase()
		_ = completeSession.Close()

		level := slog.LevelInfo
		if outcome.Kind == OutcomeRetry {
			level = slog.LevelWarn
		}
		if outcome.Kind == OutcomeFailed {
			level = slog.LevelError
		}
		log.Event(ctx, level, "webhook.delivery.attempt_completed",
			"request_id", record.RequestID, "delivery_id", id, "generation", lease.Generation,
			"attempt", lease.Attempt, "status", outcome.Kind, "error_code", outcome.ErrorCode,
			"duration_ms", time.Since(attemptStarted).Milliseconds())

		switch outcome.Kind {
		case OutcomeDelivered, OutcomeCancelled, OutcomeFailed:
			return DeliveryResult{DeliveryID: id, Status: outcome.Kind, ErrorCode: outcome.ErrorCode}
		}

		wait := outcome.RetryAt.Sub(completedAt)
		if err := deliverySleeper(ctx, wait); err != nil {
			return DeliveryResult{DeliveryID: id, Status: "error", ErrorCode: "cancelled"}
		}
		claimMode = DeliveryModeNormal
	}
}

func sendPersistedWebhook(ctx context.Context, webhook *Webhook, record *WebhookDeliveryRecord) error {
	body := record.PayloadBody
	if len(body) == 0 {
		if record.Payload == nil {
			return fmt.Errorf("webhook delivery %s has no persisted payload", record.DeliveryID)
		}
		var err error
		body, err = json.Marshal(record.Payload)
		if err != nil {
			return err
		}
	}
	return webhook.sendWebhookRawContext(ctx, body, record.RequestID)
}

// materializeDelivery freezes legacy bytes and missing UUID once while the claim
// transaction owns the row. Every later attempt reads the same values.
func materializeDelivery(s *xorm.Session, record *WebhookDeliveryRecord, lease Lease) error {
	changed := false
	if len(record.PayloadBody) == 0 {
		if record.Payload == nil {
			return errDeliveryPayloadCorrupt
		}
		body, err := json.Marshal(record.Payload)
		if err != nil {
			return err
		}
		record.PayloadBody = body
		sum := sha256.Sum256(body)
		record.PayloadSHA256 = hex.EncodeToString(sum[:])
		changed = true
	}
	id, err := uuid.Parse(record.RequestID)
	if err != nil {
		record.RequestID = uuid.NewString()
		changed = true
	} else if id.String() != record.RequestID {
		record.RequestID = id.String()
		changed = true
	}
	sum := sha256.Sum256(record.PayloadBody)
	digest := hex.EncodeToString(sum[:])
	if record.PayloadSHA256 != "" && record.PayloadSHA256 != digest {
		return errDeliveryPayloadCorrupt
	}
	if record.PayloadSHA256 == "" {
		record.PayloadSHA256 = digest
		changed = true
	}
	if !changed {
		return nil
	}
	now, clockErr := deliveryDatabaseNow(s)
	if clockErr != nil {
		return clockErr
	}
	result, err := s.Exec("UPDATE webhook_deliveries SET payload_body = ?, payload_sha256 = ?, request_id = ? WHERE delivery_id = ? AND status = 'processing' AND lease_token = ? AND retry_generation = ? AND lease_until > ?", record.PayloadBody, record.PayloadSHA256, record.RequestID, record.DeliveryID, lease.Token, lease.Generation, now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("lease lost while materializing delivery")
	}
	return nil
}

func attemptWebhookDelivery(ctx context.Context, webhook *Webhook, record *WebhookDeliveryRecord, lease Lease, cancelReason string) DeliveryOutcome {
	switch {
	case webhook == nil:
		return DeliveryOutcome{Kind: OutcomeCancelled, ErrorCode: "target_deleted"}
	case cancelReason != "":
		return DeliveryOutcome{Kind: OutcomeCancelled, ErrorCode: cancelReason}
	}
	remaining := lease.Until.Sub(lease.databaseTime) - time.Since(lease.wallStarted)
	if remaining <= 0 {
		return DeliveryOutcome{Kind: OutcomeRetry, ErrorCode: "lease_expired"}
	}
	sendCtx, cancelSend := context.WithTimeout(ctx, remaining)
	defer cancelSend()
	if err := sendPersistedWebhook(sendCtx, webhook, record); err != nil {
		if lease.Attempt >= deliveryMaxAttempt {
			return DeliveryOutcome{Kind: OutcomeFailed, ErrorCode: "delivery_failed"}
		}
		return DeliveryOutcome{Kind: OutcomeRetry, ErrorCode: "delivery_failed"}
	}
	return DeliveryOutcome{Kind: OutcomeDelivered}
}

func prepareClaimedDelivery(s *xorm.Session, id string, lease Lease) (*WebhookDeliveryRecord, *Webhook, string, error) {
	record := &WebhookDeliveryRecord{}
	has, err := s.Where("delivery_id = ?", id).Get(record)
	if err != nil {
		return nil, nil, "", err
	}
	if !has {
		return nil, nil, "", fmt.Errorf("claimed delivery not found")
	}
	webhook := &Webhook{}
	has, err = s.Where("id = ?", record.WebhookID).Get(webhook)
	if err != nil {
		return nil, nil, "", err
	}
	if err := materializeDelivery(s, record, lease); err != nil {
		return record, webhook, "", err
	}
	if !has {
		return record, nil, "", nil
	}
	reason, err := deliveryAuthorization(s, webhook, record)
	return record, webhook, reason, err
}
