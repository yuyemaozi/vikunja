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

package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"xorm.io/xorm"
)

// ErrWebhookTransactionRequired is returned when a registered transactional
// source is dispatched outside an open database transaction.
var ErrWebhookTransactionRequired = errors.New("webhook events require an open database transaction")

// PreparedEvent is the frozen event handed to a transactional sink.
type PreparedEvent struct {
	ID         string
	Name       string
	OccurredAt time.Time
	Payload    json.RawMessage
}

// TransactionalSink persists delivery snapshots inside the caller's transaction.
type TransactionalSink func(key any, event PreparedEvent, meta RequestMeta) ([]Event, error)

var transactionalSinks = struct {
	mu    sync.RWMutex
	sinks map[string]TransactionalSink
}{sinks: map[string]TransactionalSink{}}

// RegisterTransactionalSink registers one sink for the given event names.
func RegisterTransactionalSink(names []string, sink TransactionalSink) error {
	transactionalSinks.mu.Lock()
	defer transactionalSinks.mu.Unlock()
	for _, name := range names {
		if _, exists := transactionalSinks.sinks[name]; exists {
			return fmt.Errorf("transactional sink already registered for %q", name)
		}
	}
	for _, name := range names {
		transactionalSinks.sinks[name] = sink
	}
	return nil
}

func lookupTransactionalSink(name string) (TransactionalSink, bool) {
	transactionalSinks.mu.RLock()
	defer transactionalSinks.mu.RUnlock()
	sink, exists := transactionalSinks.sinks[name]
	return sink, exists
}

// resetTransactionalSinks is a test-only helper; production has no clear API.
func resetTransactionalSinks() {
	transactionalSinks.mu.Lock()
	defer transactionalSinks.mu.Unlock()
	transactionalSinks.sinks = map[string]TransactionalSink{}
}

// BindRequestMeta stores the first request UUID for a pending transaction.
// Missing or invalid metadata produces one transaction UUID; later bindings
// never replace it.
func BindRequestMeta(key any, ctx context.Context) {
	queue := pendingQueue(key)
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.requestID != "" {
		return
	}
	requestID := ""
	if meta := RequestMetaFromContext(ctx); meta != nil {
		if parsed, err := uuid.Parse(meta.RequestID); err == nil {
			requestID = parsed.String()
		}
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	queue.requestID = requestID
}

func isTransactionalKey(key any) bool {
	session, ok := key.(*xorm.Session)
	return ok && session.IsInTx()
}
