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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "vikunja")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = root
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	return binary
}

func runCLI(t *testing.T, binary string, databasePath string, arguments ...string) (int, []byte) {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Env = append(
		os.Environ(),
		"VIKUNJA_SERVICE_PUBLICURL=http://127.0.0.1:3456/",
		"VIKUNJA_DATABASE_TYPE=sqlite",
		"VIKUNJA_DATABASE_PATH="+databasePath,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.Bytes()
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		return exitError.ExitCode(), stdout.Bytes()
	}
	t.Fatalf("command failed: %v (%s)", err, stderr.String())
	return -1, nil
}

func lastJSONLine(t *testing.T, output []byte) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	for index := len(lines) - 1; index >= 0; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) > 0 && line[0] == '{' {
			return line
		}
	}
	t.Fatalf("no JSON line in output: %s", output)
	return nil
}

func TestWebhookCLIExitMatrix(t *testing.T) {
	binary := buildCLI(t)
	databasePath := filepath.Join(t.TempDir(), "cli.db")

	code, _ := runCLI(t, binary, databasePath, "webhook-deliveries", "retry", "00000000-0000-0000-0000-000000000000", "--format", "json")
	assert.Equal(t, 2, code, "missing delivery must exit 2")

	code, stdout := runCLI(t, binary, databasePath, "webhook-deliveries", "recover-pending", "--format", "json", "--limit", "1")
	assert.Equal(t, 0, code)
	var payload struct {
		Summary map[string]int `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(lastJSONLine(t, stdout), &payload))
	assert.Equal(t, 0, payload.Summary["requested"])
}
