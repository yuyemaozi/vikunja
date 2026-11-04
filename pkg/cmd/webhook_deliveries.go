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
		for _, record := range records {
			session := db.NewAutocommitSession()
			claimed, claimErr := models.ClaimWebhookDeliveryForRecovery(session, record.DeliveryID)
			_ = session.Close()
			if claimErr != nil || !claimed {
				continue
			}
			if err := models.AttemptWebhookDelivery(record.DeliveryID); err != nil {
				fmt.Fprintf(os.Stderr, "Webhook delivery %s failed: %v\n", record.DeliveryID, err)
				continue
			}
			fmt.Printf("Webhook delivery %s recovered\n", record.DeliveryID)
		}
	},
}

var webhookDeliveriesRetryCmd = &cobra.Command{
	Use:   "retry <delivery-id>",
	Short: "Requeue one failed webhook delivery",
	Args:  cobra.ExactArgs(1),
	PreRun: func(_ *cobra.Command, _ []string) {
		initialize.FullInitWithoutAsync()
	},
	Run: func(_ *cobra.Command, args []string) {
		s := db.NewAutocommitSession()
		requeued, err := models.RetryWebhookDelivery(s, args[0])
		_ = s.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not retry webhook delivery %s: %v\n", args[0], err)
			os.Exit(1)
		}
		if !requeued {
			fmt.Fprintf(os.Stderr, "Webhook delivery %s is not failed\n", args[0])
			os.Exit(1)
		}
		if err := models.AttemptWebhookDelivery(args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "Webhook delivery %s is pending but delivery failed: %v\n", args[0], err)
			os.Exit(1)
		}
		fmt.Printf("Webhook delivery %s requeued and delivered\n", args[0])
	},
}

func init() {
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
