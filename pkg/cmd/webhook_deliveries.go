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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/initialize"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"github.com/google/uuid"

	"github.com/spf13/cobra"
)

// webhookCLIError defers process exit until command-owned resources are closed.
type webhookCLIError struct {
	code    int
	message string
}

func (e *webhookCLIError) Error() string { return e.message }

func webhookParameterError(message string) error { return &webhookCLIError{2, message} }

var webhookDeliveriesCmd = &cobra.Command{
	Use:           "webhook-deliveries",
	Short:         "Inspect and retry durable webhook deliveries",
	SilenceErrors: true,
	SilenceUsage:  true,
}

func initWebhookCommand(cmd *cobra.Command, _ []string) error {
	format, _ := cmd.Flags().GetString("format")
	if format != "json" && format != "table" {
		return webhookParameterError("format must be table or json")
	}
	log.SetOutputOverride(os.Stderr)
	initialize.FullInitWithoutAsync()
	return nil
}

var webhookDeliveriesListFormat string
var webhookDeliveriesListCmd = &cobra.Command{
	Use: "list", Short: "List pending and failed webhook deliveries",
	Args:    cobra.NoArgs,
	PreRunE: initWebhookCommand,
	RunE: func(cmd *cobra.Command, _ []string) error {
		queryCtx, cancelQuery := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancelQuery()
		s := db.NewReadSession()
		db.SetSessionContext(queryCtx, s)
		defer s.Close()
		records, err := models.ListWebhookDeliveries(s, nil)
		if err != nil {
			return &webhookCLIError{1, "Could not list webhook deliveries: db_error"}
		}
		if webhookDeliveriesListFormat == "json" {
			payload, err := webhookDeliveriesJSON(records)
			if err != nil {
				return &webhookCLIError{1, "Could not encode webhook deliveries"}
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), string(payload)); err != nil {
				return &webhookCLIError{1, "Could not write webhook deliveries"}
			}
			return nil
		}
		printWebhookDeliveries(cmd.OutOrStdout(), records)
		return nil
	},
}

var (
	webhookDeliveriesRecoverLimit   int
	webhookDeliveriesRecoverTimeout int
	webhookDeliveriesRecoverFormat  string
)
var webhookDeliveriesRecoverCmd = &cobra.Command{
	Use: "recover-pending", Aliases: []string{"recover"},
	Short: "Claim and deliver stranded pending webhook deliveries",
	Args:  cobra.NoArgs,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if webhookDeliveriesRecoverLimit < 1 || webhookDeliveriesRecoverLimit > 1000 {
			return webhookParameterError("limit must be between 1 and 1000")
		}
		if webhookDeliveriesRecoverTimeout < 1 || webhookDeliveriesRecoverTimeout > 3600 {
			return webhookParameterError("timeout must be between 1 and 3600 seconds")
		}
		return initWebhookCommand(cmd, args)
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(webhookDeliveriesRecoverTimeout)*time.Second)
		defer cancel()
		queryCtx, cancelQuery := context.WithTimeout(ctx, 5*time.Second)
		s := db.NewReadSession()
		db.SetSessionContext(queryCtx, s)
		// The database clock owns recovery eligibility.
		records, err := models.RecoverableWebhookDeliveries(s, time.Time{}, webhookDeliveriesRecoverLimit)
		_ = s.Close()
		cancelQuery()
		if err != nil {
			return writeDeliveryResults(cmd, []models.DeliveryResult{}, "db_error")
		}
		results := make([]models.DeliveryResult, 0, len(records))
		for _, record := range records {
			result := models.DeliveryResult{DeliveryID: record.DeliveryID, Status: "error", ErrorCode: "context_cancelled"}
			if ctx.Err() == nil {
				result = models.RunWebhookDelivery(ctx, record.DeliveryID, models.DeliveryModeRecover)
			}
			results = append(results, result)
		}
		return writeDeliveryResults(cmd, results, "")
	},
}

var webhookDeliveriesRetryFormat string
var webhookDeliveriesRetryCmd = &cobra.Command{
	Use: "retry <delivery-id>", Short: "Requeue one failed webhook delivery",
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return webhookParameterError("retry requires one delivery UUID")
		}
		if _, err := uuid.Parse(args[0]); err != nil {
			return webhookParameterError("delivery ID must be a UUID")
		}
		return nil
	},
	PreRunE: initWebhookCommand,
	RunE: func(cmd *cobra.Command, args []string) error {
		result := models.RunWebhookDelivery(cmd.Context(), args[0], models.DeliveryModeRetry)
		return writeDeliveryResults(cmd, []models.DeliveryResult{result}, "")
	},
}

// Every selected item contributes to exactly one result category.
func deliveryResultsJSON(results []models.DeliveryResult, batchError string) ([]byte, int, error) {
	summary := map[string]int{"selected": len(results), "delivered": 0, "cancelled": 0, "already_resolved": 0, "busy": 0, "failed": 0, "errors": 0}
	items := make([]map[string]string, 0, len(results))
	code := 0
	for _, result := range results {
		items = append(items, map[string]string{"delivery_id": result.DeliveryID, "status": result.Status, "error_code": result.ErrorCode})
		switch result.Status {
		case "delivered", "cancelled", "already_resolved":
			summary[result.Status]++
		case "busy":
			summary["busy"]++
			if code == 0 {
				code = 2
			}
		case "not_found":
			summary["errors"]++
			if code == 0 {
				code = 2
			}
		case "failed":
			summary["failed"]++
			code = 1
		default:
			summary["errors"]++
			code = 1
		}
	}
	if batchError != "" {
		summary["errors"]++
		code = 1
	}
	payload, err := json.Marshal(map[string]any{"summary": summary, "items": items})
	return payload, code, err
}

func writeDeliveryResults(cmd *cobra.Command, results []models.DeliveryResult, batchError string) error {
	payload, code, err := deliveryResultsJSON(results, batchError)
	if err != nil {
		return &webhookCLIError{1, "Could not encode webhook results"}
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(payload)); err != nil {
		return &webhookCLIError{1, "Could not write webhook results"}
	}
	if code != 0 {
		return &webhookCLIError{code, "Webhook delivery command did not resolve all selected deliveries"}
	}
	return nil
}

func init() {
	for _, command := range []*cobra.Command{webhookDeliveriesListCmd, webhookDeliveriesRetryCmd, webhookDeliveriesRecoverCmd} {
		command.SilenceErrors = true
		command.SilenceUsage = true
	}
	webhookDeliveriesRecoverCmd.Flags().IntVar(&webhookDeliveriesRecoverLimit, "limit", 100, "maximum deliveries in this batch (1-1000)")
	webhookDeliveriesRecoverCmd.Flags().IntVar(&webhookDeliveriesRecoverTimeout, "timeout", 300, "batch timeout in seconds (1-3600)")
	webhookDeliveriesRecoverCmd.Flags().StringVar(&webhookDeliveriesRecoverFormat, "format", "table", "output format: table or json")
	webhookDeliveriesRetryCmd.Flags().StringVar(
		&webhookDeliveriesRetryFormat,
		"format",
		"table",
		"output format: table or json",
	)
	webhookDeliveriesListCmd.Flags().StringVar(
		&webhookDeliveriesListFormat,
		"format",
		"table",
		"output format: table or json",
	)
	rootCmd.AddCommand(webhookDeliveriesCmd)
	webhookDeliveriesCmd.AddCommand(webhookDeliveriesListCmd, webhookDeliveriesRetryCmd, webhookDeliveriesRecoverCmd)
}

func printWebhookDeliveries(out io.Writer, records []*models.WebhookDeliveryRecord) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "DELIVERY ID\tEVENT\tSTATUS\tATTEMPTS\tGENERATION\tLEASE UNTIL\tERROR\tCREATED\tLAST ATTEMPT")
	for _, record := range records {
		lastAttempt := "-"
		leaseUntil := "-"
		if record.LeaseUntil != nil {
			leaseUntil = record.LeaseUntil.UTC().Format(time.RFC3339)
		}
		if record.LastAttemptAt != nil {
			lastAttempt = record.LastAttemptAt.Format("2006-01-02T15:04:05Z07:00")
		}
		_, _ = fmt.Fprintf(
			w,
			"%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			record.DeliveryID,
			record.EventName,
			record.Status,
			record.AttemptCount,
			record.RetryGeneration, leaseUntil, record.LastErrorCode,
			record.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			lastAttempt,
		)
	}
	_ = w.Flush()
}

type webhookDeliveryJSON struct {
	DeliveryID    string `json:"delivery_id"`
	Event         string `json:"event"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	CreatedAt     string `json:"created_at"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastErrorCode string `json:"last_error_code,omitempty"`
	Generation    int    `json:"generation"`
	LeaseUntil    string `json:"lease_until,omitempty"`
}

func webhookDeliveriesJSON(records []*models.WebhookDeliveryRecord) ([]byte, error) {
	items := make([]webhookDeliveryJSON, 0, len(records))
	for _, record := range records {
		item := webhookDeliveryJSON{
			DeliveryID:    record.DeliveryID,
			Generation:    record.RetryGeneration,
			Event:         record.EventName,
			Status:        record.Status,
			Attempts:      record.AttemptCount,
			CreatedAt:     record.CreatedAt.UTC().Format(time.RFC3339),
			LastErrorCode: record.LastErrorCode,
		}
		if record.LeaseUntil != nil {
			item.LeaseUntil = record.LeaseUntil.UTC().Format(time.RFC3339)
		}
		if record.LastAttemptAt != nil {
			item.LastAttemptAt = record.LastAttemptAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	return json.Marshal(map[string]any{"deliveries": items})
}

func deliveryResultJSON(result models.DeliveryResult) ([]byte, error) {
	payload, _, err := deliveryResultsJSON([]models.DeliveryResult{result}, "")
	return payload, err
}
