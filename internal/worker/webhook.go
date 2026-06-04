package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/hibiken/asynq"

	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/queue"
)

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

var webhookDelays = []time.Duration{
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
}

// WebhookPayload defines the payload sent to webhook endpoints.
type WebhookPayload struct {
	JobID         string    `json:"job_id"`
	Status        string    `json:"status"`
	StorageURL    *string   `json:"storage_url"`
	FailureReason *string   `json:"failure_reason"`
	Timestamp     time.Time `json:"timestamp"`
}

// FireWebhooksForJob finds all active webhook endpoints for the job's developer
// and sends the webhook payload to them asynchronously.
func FireWebhooksForJob(jobID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if db.DB == nil {
			log.Printf("webhook fire: database connection is nil")
			return
		}

		// 1. Fetch job status and developer ID
		var jobStatus string
		var storageURL sql.NullString
		var failureReason sql.NullString
		var developerID string

		err := db.DB.QueryRowContext(ctx, `
			SELECT status, storage_url, failure_reason, developer_id
			FROM upload_jobs
			WHERE id = $1
		`, jobID).Scan(&jobStatus, &storageURL, &failureReason, &developerID)
		if err != nil {
			log.Printf("webhook fire: failed to fetch job %s details: %v", jobID, err)
			return
		}

		// 2. Fetch all active webhook endpoints for this developer
		rows, err := db.DB.QueryContext(ctx, `
			SELECT id, url, secret
			FROM webhook_endpoints
			WHERE developer_id = $1 AND is_active = true
		`, developerID)
		if err != nil {
			log.Printf("webhook fire: failed to fetch endpoints for developer %s: %v", developerID, err)
			return
		}
		defer rows.Close()

		type endpoint struct {
			id     string
			url    string
			secret string
		}
		var endpoints []endpoint
		for rows.Next() {
			var ep endpoint
			if err := rows.Scan(&ep.id, &ep.url, &ep.secret); err == nil {
				endpoints = append(endpoints, ep)
			}
		}

		if len(endpoints) == 0 {
			return // No endpoints registered
		}

		// Prepare the payload
		var sURL *string
		if storageURL.Valid {
			sURL = &storageURL.String
		}
		var fReason *string
		if failureReason.Valid {
			fReason = &failureReason.String
		}

		payload := WebhookPayload{
			JobID:         jobID,
			Status:        jobStatus,
			StorageURL:    sURL,
			FailureReason: fReason,
			Timestamp:     time.Now(),
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			log.Printf("webhook fire: failed to marshal payload: %v", err)
			return
		}

		// 3. Dispatch to each endpoint in parallel
		for _, ep := range endpoints {
			go fireSingleWebhook(context.Background(), jobID, ep.id, ep.url, ep.secret, payloadBytes)
		}
	}()
}

func fireSingleWebhook(ctx context.Context, jobID, endpointID, url, secret string, payloadBytes []byte) {
	// Create delivery record with status 'pending'
	var deliveryID string
	err := db.DB.QueryRowContext(ctx, `
		INSERT INTO webhook_deliveries (upload_job_id, webhook_endpoint_id, status)
		VALUES ($1, $2, 'pending')
		RETURNING id
	`, jobID, endpointID).Scan(&deliveryID)
	if err != nil {
		log.Printf("webhook fire: failed to insert pending delivery: %v", err)
		return
	}

	// Compute HMAC signature
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payloadBytes)
	signature := hex.EncodeToString(h.Sum(nil))

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payloadBytes))
	if err != nil {
		log.Printf("webhook fire: failed to create request for %s: %v", url, err)
		handleFailedAttempt(ctx, deliveryID, 1, 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("webhook fire: delivery to %s failed: %v", url, err)
		handleFailedAttempt(ctx, deliveryID, 1, 0)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		updateDeliveryStatus(ctx, deliveryID, "delivered", 1, resp.StatusCode, true)
	} else {
		log.Printf("webhook fire: delivery to %s returned non-2xx status: %d", url, resp.StatusCode)
		handleFailedAttempt(ctx, deliveryID, 1, resp.StatusCode)
	}
}

func updateDeliveryStatus(ctx context.Context, deliveryID, status string, attemptCount, respStatus int, success bool) {
	var err error
	if success {
		_, err = db.DB.ExecContext(ctx, `
			UPDATE webhook_deliveries
			SET status = $1, attempt_count = $2, last_attempted_at = now(), delivered_at = now(), response_status = $3
			WHERE id = $4
		`, status, attemptCount, respStatus, deliveryID)
	} else {
		var respStatusVal interface{}
		if respStatus > 0 {
			respStatusVal = respStatus
		} else {
			respStatusVal = nil
		}
		_, err = db.DB.ExecContext(ctx, `
			UPDATE webhook_deliveries
			SET status = $1, attempt_count = $2, last_attempted_at = now(), response_status = $3
			WHERE id = $4
		`, status, attemptCount, respStatusVal, deliveryID)
	}

	if err != nil {
		log.Printf("webhook fire: failed to update delivery %s status: %v", deliveryID, err)
	}
}

func handleFailedAttempt(ctx context.Context, deliveryID string, currentAttemptCount int, responseStatus int) {
	var respStatusVal interface{}
	if responseStatus > 0 {
		respStatusVal = responseStatus
	} else {
		respStatusVal = nil
	}

	// Update database with latest failed attempt details
	_, err := db.DB.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET attempt_count = $1, last_attempted_at = now(), response_status = $2
		WHERE id = $3
	`, currentAttemptCount, respStatusVal, deliveryID)
	if err != nil {
		log.Printf("webhook retry: failed to update delivery %s attempt info: %v", deliveryID, err)
	}

	if currentAttemptCount <= len(webhookDelays) {
		delay := webhookDelays[currentAttemptCount-1]
		err = queue.EnqueueWebhookRetry(deliveryID, delay)
		if err != nil {
			log.Printf("webhook retry: failed to enqueue retry for delivery %s: %v", deliveryID, err)
		}
	} else {
		// No more retries (e.g. attempt 5 failed), mark delivery status as 'failed'
		_, err = db.DB.ExecContext(ctx, `
			UPDATE webhook_deliveries
			SET status = 'failed'
			WHERE id = $1
		`, deliveryID)
		if err != nil {
			log.Printf("webhook retry: failed to mark delivery %s as failed: %v", deliveryID, err)
		}
	}
}

// HandleWebhookRetry handles retrying webhook deliveries.
func HandleWebhookRetry(ctx context.Context, t *asynq.Task) error {
	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal webhook retry payload: %w", err)
	}

	deliveryID := payload["delivery_id"]
	if deliveryID == "" {
		return fmt.Errorf("missing delivery_id in task payload")
	}

	if db.DB == nil {
		return fmt.Errorf("database connection is not initialized")
	}

	// 1. Fetch delivery details
	var jobID string
	var endpointID string
	var currentAttemptCount int
	var deliveryStatus string

	err := db.DB.QueryRowContext(ctx, `
		SELECT upload_job_id, webhook_endpoint_id, attempt_count, status
		FROM webhook_deliveries
		WHERE id = $1
	`, deliveryID).Scan(&jobID, &endpointID, &currentAttemptCount, &deliveryStatus)
	if err != nil {
		return fmt.Errorf("failed to fetch delivery %s: %w", deliveryID, err)
	}

	if deliveryStatus == "delivered" {
		return nil
	}

	// 2. Fetch endpoint URL and secret
	var url string
	var secret string
	err = db.DB.QueryRowContext(ctx, `
		SELECT url, secret
		FROM webhook_endpoints
		WHERE id = $1 AND is_active = true
	`, endpointID).Scan(&url, &secret)
	if err != nil {
		_, _ = db.DB.ExecContext(ctx, `
			UPDATE webhook_deliveries
			SET status = 'failed'
			WHERE id = $1
		`, deliveryID)
		return fmt.Errorf("webhook endpoint %s not active or not found: %w", endpointID, err)
	}

	// 3. Fetch job details for payload
	var jobStatus string
	var storageURL sql.NullString
	var failureReason sql.NullString
	err = db.DB.QueryRowContext(ctx, `
		SELECT status, storage_url, failure_reason
		FROM upload_jobs
		WHERE id = $1
	`, jobID).Scan(&jobStatus, &storageURL, &failureReason)
	if err != nil {
		return fmt.Errorf("failed to fetch job %s details for retry: %w", jobID, err)
	}

	var sURL *string
	if storageURL.Valid {
		sURL = &storageURL.String
	}
	var fReason *string
	if failureReason.Valid {
		fReason = &failureReason.String
	}

	webhookPayload := WebhookPayload{
		JobID:         jobID,
		Status:        jobStatus,
		StorageURL:    sURL,
		FailureReason: fReason,
		Timestamp:     time.Now(),
	}

	payloadBytes, err := json.Marshal(webhookPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal webhook payload: %w", err)
	}

	// 4. Fire retry attempt (this counts as attempt currentAttemptCount + 1)
	nextAttempt := currentAttemptCount + 1

	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payloadBytes)
	signature := hex.EncodeToString(h.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payloadBytes))
	if err != nil {
		handleFailedAttempt(ctx, deliveryID, nextAttempt, 0)
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", signature)

	resp, err := httpClient.Do(req)
	if err != nil {
		handleFailedAttempt(ctx, deliveryID, nextAttempt, 0)
		return fmt.Errorf("webhook retry attempt %d failed: %w", nextAttempt, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		updateDeliveryStatus(ctx, deliveryID, "delivered", nextAttempt, resp.StatusCode, true)
	} else {
		handleFailedAttempt(ctx, deliveryID, nextAttempt, resp.StatusCode)
		return fmt.Errorf("webhook retry attempt %d returned status: %d", nextAttempt, resp.StatusCode)
	}

	return nil
}
