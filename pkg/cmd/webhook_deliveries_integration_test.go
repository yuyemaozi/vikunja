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
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "vikunja")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Dir = root
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	return binary
}

func runCLI(t *testing.T, binary string, databasePath string, arguments ...string) (int, []byte) {
	t.Helper()
	code, stdout, _ := runCLICaptured(t, binary, databasePath, arguments...)
	return code, stdout
}

func runCLICaptured(t *testing.T, binary string, databasePath string, arguments ...string) (int, []byte, []byte) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binary, arguments...)
	command.Env = append(
		os.Environ(),
		"VIKUNJA_SERVICE_PUBLICURL=http://127.0.0.1:3456/",
		"VIKUNJA_DATABASE_TYPE=sqlite",
		"VIKUNJA_DATABASE_PATH="+databasePath,
		"VIKUNJA_OUTGOINGREQUESTS_ALLOWNONROUTABLEIPS=true",
		"VIKUNJA_WEBHOOKS_ENABLED=true",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.Bytes(), stderr.Bytes()
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		return exitError.ExitCode(), stdout.Bytes(), stderr.Bytes()
	}
	t.Fatalf("command failed: %v (%s)", err, stderr.String())
	return -1, nil, nil
}

func TestWebhookCLIExitMatrix(t *testing.T) {
	binary := buildCLI(t)
	databasePath := filepath.Join(t.TempDir(), "cli.db")

	code, missing := runCLI(t, binary, databasePath, "webhook-deliveries", "retry", "00000000-0000-0000-0000-000000000000", "--format", "json")
	assert.Equal(t, 2, code, "missing delivery must exit 2")
	require.True(t, json.Valid(missing), string(missing))

	database, err := sql.Open("sqlite3", databasePath)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), "INSERT INTO users (id,username,email,status,created,updated) VALUES (1,'synthetic-cli','synthetic@example.invalid',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)")
	require.NoError(t, err)
	const busyID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10f"
	_, err = database.ExecContext(t.Context(),
		"INSERT INTO webhook_deliveries (delivery_id, webhook_id, event_name, payload, status, lease_token, lease_until, retry_generation, cycle_attempt_count, created_at) "+
			"VALUES (?, 1, 'task.reminder.fired', '{}', 'processing', 'lease', ?, 0, 1, CURRENT_TIMESTAMP)",
		busyID, time.Now().Add(time.Hour).UTC(),
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	code, busy := runCLI(t, binary, databasePath, "webhook-deliveries", "retry", busyID, "--format", "json")
	assert.Equal(t, 2, code, "a valid lease must exit 2")
	require.True(t, json.Valid(busy), string(busy))
	code, listing := runCLI(t, binary, databasePath, "webhook-deliveries", "list", "--format", "json")
	assert.Equal(t, 0, code)
	var listed struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	require.NoError(t, json.Unmarshal(listing, &listed))
	require.Len(t, listed.Deliveries, 1)
	assert.Equal(t, busyID, listed.Deliveries[0]["delivery_id"])
	assert.Equal(t, "processing", listed.Deliveries[0]["status"])

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	database, err = sql.Open("sqlite3", databasePath)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(),
		"INSERT INTO webhooks (id, target_url, events, user_id, created_by_id, created, updated) "+
			`VALUES (1, ?, '["task.reminder.fired"]', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		server.URL,
	)
	require.NoError(t, err)
	const failingID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10a"
	_, err = database.ExecContext(t.Context(),
		"INSERT INTO webhook_deliveries (delivery_id, webhook_id, event_name, payload, status, retry_generation, cycle_attempt_count, created_at) "+
			`VALUES (?, 1, 'task.reminder.fired', '{"event_name":"task.reminder.fired","time":"2026-11-04T00:00:00Z","data":{}}', 'failed', 0, 6, CURRENT_TIMESTAMP)`,
		failingID,
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	code, _ = runCLI(t, binary, databasePath, "webhook-deliveries", "retry", failingID, "--format", "json")
	assert.Equal(t, 1, code)
	assert.EqualValues(t, 6, requests.Load())
	database, err = sql.Open("sqlite3", databasePath)
	require.NoError(t, err)
	var status string
	var attempts int
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT status, cycle_attempt_count FROM webhook_deliveries WHERE delivery_id = ?", failingID).Scan(&status, &attempts))
	assert.Equal(t, "failed", status)
	assert.Equal(t, 6, attempts)
	require.NoError(t, database.Close())

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	database, err = sql.Open("sqlite3", databasePath)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(),
		"INSERT INTO webhooks (id, target_url, events, user_id, created_by_id, created, updated) "+
			`VALUES (2, ?, '["task.reminder.fired"]', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		good.URL,
	)
	require.NoError(t, err)

	const pendingID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e250"
	_, err = database.ExecContext(t.Context(), `INSERT INTO webhook_deliveries (delivery_id,webhook_id,event_name,payload,status,retry_generation,cycle_attempt_count,attempt_count,created_at) VALUES (?,2,'task.reminder.fired','{"event_name":"task.reminder.fired","time":"2026-11-04T00:00:00Z","data":{}}','pending',3,2,8,CURRENT_TIMESTAMP)`, pendingID)
	require.NoError(t, err)
	code, pendingOutput := runCLI(t, binary, databasePath, "webhook-deliveries", "retry", pendingID)
	assert.Equal(t, 0, code)
	require.True(t, json.Valid(pendingOutput), string(pendingOutput))
	var generation, cycleAttempts, totalAttempts int
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT status,retry_generation,cycle_attempt_count,attempt_count FROM webhook_deliveries WHERE delivery_id = ?", pendingID).Scan(&status, &generation, &cycleAttempts, &totalAttempts))
	assert.Equal(t, "delivered", status)
	assert.Equal(t, 3, generation)
	assert.Equal(t, 3, cycleAttempts)
	assert.Equal(t, 9, totalAttempts)
	for index, webhookID := range []int{1, 2} {
		_, err = database.ExecContext(t.Context(),
			"INSERT INTO webhook_deliveries (delivery_id, webhook_id, event_name, payload, status, retry_generation, cycle_attempt_count, created_at) "+
				`VALUES (?, ?, 'task.reminder.fired', '{"event_name":"task.reminder.fired","time":"2026-11-04T00:00:00Z","data":{}}', 'pending', 0, 0, datetime('now','-10 minutes'))`,
			"018f8f4c-2e18-7b61-9a50-9c4f5de4e20"+string(rune('0'+index)),
			webhookID,
		)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())

	code, partialStdout := runCLI(t, binary, databasePath, "webhook-deliveries", "recover-pending", "--format", "json")
	assert.Equal(t, 1, code)
	var partial struct {
		Summary map[string]int `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(partialStdout, &partial))
	assert.Equal(t, 2, partial.Summary["selected"])
	assert.Equal(t, 1, partial.Summary["failed"])
	assert.Equal(t, 1, partial.Summary["delivered"])
	assert.Zero(t, partial.Summary["errors"])
	database, err = sql.Open("sqlite3", databasePath)
	require.NoError(t, err)
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT status FROM webhook_deliveries WHERE delivery_id = ?", "018f8f4c-2e18-7b61-9a50-9c4f5de4e201").Scan(&status))
	assert.Equal(t, "delivered", status)
	require.NoError(t, database.Close())
	code, repeated := runCLI(t, binary, databasePath, "webhook-deliveries", "retry", "018f8f4c-2e18-7b61-9a50-9c4f5de4e201")
	assert.Equal(t, 0, code)
	assert.Contains(t, string(repeated), "already_resolved")

	code, stdout := runCLI(t, binary, databasePath, "webhook-deliveries", "recover-pending", "--format", "json", "--limit", "1")
	assert.Equal(t, 0, code)
	var payload struct {
		Summary map[string]int `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(stdout, &payload))
	assert.Equal(t, 0, payload.Summary["selected"])
}

func TestWebhookCLIInvalidParameters(t *testing.T) {
	binary := buildCLI(t)
	for _, arguments := range [][]string{
		{"retry", "private-note-secret"},
		{"retry"},
		{"recover", "--limit", "0"},
		{"recover-pending", "--timeout", "3601"},
		{"list", "--format", "invalid"},
		{"recover", "--limit", "private-note-secret"},
		{"list", "private-note-secret"},
	} {
		t.Run(arguments[0]+arguments[len(arguments)-1], func(t *testing.T) {
			code, output, stderr := runCLICaptured(t, binary, filepath.Join(t.TempDir(), "cli.db"), append([]string{"webhook-deliveries"}, arguments...)...)
			assert.Equal(t, 2, code)
			assert.Empty(t, output)
			assert.NotContains(t, string(stderr), "private-note-secret")
		})
	}
}

func TestWebhookCLIDatabaseReadFailure(t *testing.T) {
	binary := buildCLI(t)
	path := filepath.Join(t.TempDir(), "cli.db")
	code, output := runCLI(t, binary, path, "webhook-deliveries", "list", "--format", "json")
	require.Equal(t, 0, code)
	require.True(t, json.Valid(output), string(output))
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), "DROP TABLE webhook_deliveries")
	require.NoError(t, err)
	require.NoError(t, database.Close())
	for _, arguments := range [][]string{
		{"list", "--format", "json"},
		{"retry", "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"},
		{"recover"},
	} {
		code, output = runCLI(t, binary, path, append([]string{"webhook-deliveries"}, arguments...)...)
		assert.Equal(t, 1, code)
		if arguments[0] != "list" {
			require.True(t, json.Valid(output), string(output))
		}
	}
}

func TestWebhookCLIBatchDeadline(t *testing.T) {
	binary := buildCLI(t)
	path := filepath.Join(t.TempDir(), "cli.db")
	code, _ := runCLI(t, binary, path, "webhook-deliveries", "list", "--format", "json")
	require.Equal(t, 0, code)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer server.Close()
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer database.Close()
	_, err = database.ExecContext(t.Context(), "INSERT INTO users (id,username,email,status,created,updated) VALUES (1,'synthetic-cli','synthetic@example.invalid',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)")
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `INSERT INTO webhooks (id,target_url,events,user_id,created_by_id,created,updated) VALUES (1,?,'["task.reminder.fired"]',1,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, server.URL)
	require.NoError(t, err)
	for _, id := range []string{"018f8f4c-2e18-7b61-9a50-9c4f5de4e301", "018f8f4c-2e18-7b61-9a50-9c4f5de4e302"} {
		_, err = database.ExecContext(t.Context(), `INSERT INTO webhook_deliveries (delivery_id,webhook_id,event_name,payload,status,created_at) VALUES (?,1,'task.reminder.fired','{"event_name":"task.reminder.fired","time":"2026-11-04T00:00:00Z","data":{}}','pending',datetime('now','-10 minutes'))`, id)
		require.NoError(t, err)
	}
	started := time.Now()
	code, output := runCLI(t, binary, path, "webhook-deliveries", "recover", "--timeout", "1")
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.Equal(t, 1, code)
	var result struct {
		Summary map[string]int      `json:"summary"`
		Items   []map[string]string `json:"items"`
	}
	require.NoError(t, json.Unmarshal(output, &result), string(output))
	assert.Equal(t, 2, result.Summary["selected"])
	assert.Equal(t, 2, result.Summary["errors"])
	assert.EqualValues(t, 1, requests.Load())
	var status string
	var attempts int
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT status, attempt_count FROM webhook_deliveries WHERE delivery_id = ?", "018f8f4c-2e18-7b61-9a50-9c4f5de4e302").Scan(&status, &attempts))
	assert.Equal(t, "pending", status)
	assert.Zero(t, attempts)
}

func TestWebhookCLIRecoverAliasAndLimit(t *testing.T) {
	binary := buildCLI(t)
	path := filepath.Join(t.TempDir(), "cli.db")
	code, _ := runCLI(t, binary, path, "webhook-deliveries", "list", "--format", "json")
	require.Equal(t, 0, code)
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer database.Close()
	_, err = database.ExecContext(t.Context(), "INSERT INTO users (id,username,email,status,created,updated) VALUES (1,'synthetic-cli','synthetic@example.invalid',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)")
	require.NoError(t, err)
	// No matching webhook: selected rows are durably cancelled; failed is never recovered.
	for index, status := range []string{"pending", "pending", "failed"} {
		_, err = database.ExecContext(t.Context(), `INSERT INTO webhook_deliveries (delivery_id,webhook_id,event_name,payload,status,created_at) VALUES (?,999,'task.reminder.fired','{}',?,datetime('now','-10 minutes'))`,
			"018f8f4c-2e18-7b61-9a50-9c4f5de4e40"+string(rune('1'+index)), status)
		require.NoError(t, err)
	}
	code, output := runCLI(t, binary, path, "webhook-deliveries", "recover", "--limit", "1")
	require.Equal(t, 0, code)
	var result struct {
		Summary map[string]int      `json:"summary"`
		Items   []map[string]string `json:"items"`
	}
	require.NoError(t, json.Unmarshal(output, &result))
	assert.Equal(t, 1, result.Summary["selected"])
	assert.Equal(t, 1, result.Summary["cancelled"])
	var status string
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT status FROM webhook_deliveries WHERE delivery_id = ?", result.Items[0]["delivery_id"]).Scan(&status))
	assert.Equal(t, "cancelled", status)
	code, output = runCLI(t, binary, path, "webhook-deliveries", "retry", result.Items[0]["delivery_id"])
	assert.Equal(t, 0, code)
	require.NoError(t, json.Unmarshal(output, &result))
	assert.Equal(t, 1, result.Summary["already_resolved"])
	code, output = runCLI(t, binary, path, "webhook-deliveries", "recover-pending")
	assert.Equal(t, 0, code)
	require.NoError(t, json.Unmarshal(output, &result))
	assert.Equal(t, 1, result.Summary["selected"])
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT status FROM webhook_deliveries WHERE delivery_id = '018f8f4c-2e18-7b61-9a50-9c4f5de4e403'").Scan(&status))
	assert.Equal(t, "failed", status)
	code, output = runCLI(t, binary, path, "webhook-deliveries", "list", "--format", "json")
	assert.Equal(t, 0, code)
	var listed struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	require.NoError(t, json.Unmarshal(output, &listed))
	require.Len(t, listed.Deliveries, 1)
	assert.Contains(t, listed.Deliveries[0], "generation")
}
