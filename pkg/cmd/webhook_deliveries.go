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
	"code.vikunja.io/api/pkg/models"

	"github.com/spf13/cobra"
)

var webhookDeliveriesCmd = &cobra.Command{
	Use:   "webhook-deliveries",
	Short: "Inspect and retry durable webhook deliveries",
}

var webhookDeliveriesListFormat string

var webhookDeliveriesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending and failed webhook deliveries",
	PreRun: func(_ *cobra.Command, _ []string) {
		initialize.FullInitWithoutAsync()
	},
	Run: func(_ *cobra.Command, _ []string) {
		s := db.NewSession()
		defer s.Close()
		records, err := models.ListWebhookDeliveries(s, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not list webhook deliveries: %v\n", err)
			os.Exit(1)
		}
		if webhookDeliveriesListFormat == "json" {
			payload, err := webhookDeliveriesJSON(records)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Could not encode webhook deliveries: %v\n", err)
				os.Exit(1)
			}
			_, _ = fmt.Fprintln(os.Stdout, string(payload))
			return
		}
		printWebhookDeliveries(os.Stdout, records)
	},
}

var webhookDeliveriesRecoverCmd = &cobra.Command{
	Use:   "recover",
	Short: "Claim and deliver pending webhook deliveries older than a cutoff",
	PreRun: func(_ *cobra.Command, _ []string) {
		initialize.FullInitWithoutAsync()
	},
	Run: func(_ *cobra.Command, _ []string) {
		s := db.NewAutocommitSession()
		records, err := models.RecoverableWebhookDeliveries(s, time.Now().Add(-5*time.Minute))
		_ = s.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not list pending webhook deliveries: %v\n", err)
			os.Exit(1)
		}
		failures := 0
		for _, record := range records {
			result := models.RunWebhookDelivery(context.Background(), record.DeliveryID, models.DeliveryModeRecover)
			if result.Status == "error" || result.Status == "failed" {
				failures++
				fmt.Fprintf(os.Stderr, "Webhook delivery %s %s\n", record.DeliveryID, result.Status)
				continue
			}
			fmt.Printf("Webhook delivery %s %s\n", record.DeliveryID, result.Status)
		}
		if failures > 0 {
			os.Exit(1)
		}
	},
}

var webhookDeliveriesRetryFormat string

var webhookDeliveriesRetryCmd = &cobra.Command{
	Use:   "retry <delivery-id>",
	Short: "Requeue one failed webhook delivery",
	Args:  cobra.ExactArgs(1),
	PreRun: func(_ *cobra.Command, _ []string) {
		initialize.FullInitWithoutAsync()
	},
	Run: func(_ *cobra.Command, args []string) {
		result := models.RunWebhookDelivery(context.Background(), args[0], models.DeliveryModeRetry)
		if webhookDeliveriesRetryFormat == "json" {
			payload, err := deliveryResultJSON(result)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Could not encode delivery result: %v\n", err)
				os.Exit(1)
			}
			_, _ = fmt.Fprintln(os.Stdout, string(payload))
		}
		switch result.Status {
		case "delivered", "cancelled", "already_resolved":
			if webhookDeliveriesRetryFormat != "json" {
				fmt.Printf("Webhook delivery %s %s\n", args[0], result.Status)
			}
		case "not_found", "busy":
			fmt.Fprintf(os.Stderr, "Webhook delivery %s %s\n", args[0], result.Status)
			os.Exit(2)
		default:
			fmt.Fprintf(os.Stderr, "Webhook delivery %s %s\n", args[0], result.Status)
			os.Exit(1)
		}
	},
}

func init() {
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
	_, _ = fmt.Fprintln(w, "DELIVERY ID\tEVENT\tSTATUS\tATTEMPTS\tCREATED\tLAST ATTEMPT")
	for _, record := range records {
		lastAttempt := "-"
		if record.LastAttemptAt != nil {
			lastAttempt = record.LastAttemptAt.Format("2006-01-02T15:04:05Z07:00")
		}
		_, _ = fmt.Fprintf(
			w,
			"%s\t%s\t%s\t%d\t%s\t%s\n",
			record.DeliveryID,
			record.EventName,
			record.Status,
			record.AttemptCount,
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
}

func webhookDeliveriesJSON(records []*models.WebhookDeliveryRecord) ([]byte, error) {
	items := make([]webhookDeliveryJSON, 0, len(records))
	for _, record := range records {
		item := webhookDeliveryJSON{
			DeliveryID:    record.DeliveryID,
			Event:         record.EventName,
			Status:        record.Status,
			Attempts:      record.AttemptCount,
			CreatedAt:     record.CreatedAt.UTC().Format(time.RFC3339),
			LastErrorCode: record.LastErrorCode,
		}
		if record.LastAttemptAt != nil {
			item.LastAttemptAt = record.LastAttemptAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	return json.Marshal(map[string]any{"deliveries": items})
}

func deliveryResultJSON(result models.DeliveryResult) ([]byte, error) {
	item := map[string]any{
		"delivery_id": result.DeliveryID,
		"status":      result.Status,
	}
	if result.ErrorCode != "" {
		item["error_code"] = result.ErrorCode
	}
	return json.Marshal(map[string]any{
		"summary": map[string]any{
			"requested": 1,
			"delivered": boolToInt(result.Status == "delivered" || result.Status == "cancelled"),
			"failed":    boolToInt(result.Status == "failed"),
			"errors":    boolToInt(result.Status == "error"),
		},
		"items": []map[string]any{item},
	})
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
