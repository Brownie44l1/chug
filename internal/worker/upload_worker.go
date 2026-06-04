package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	"github.com/Brownie44l1/chug/internal/config"
	"github.com/Brownie44l1/chug/internal/db"
	"github.com/Brownie44l1/chug/internal/storage"
)

type Uploader interface {
	Upload(ctx context.Context, input storage.UploadInput) (*storage.UploadOutput, error)
}

var uploader Uploader

var getRetryInfo = func(ctx context.Context) (retryCount int, maxRetry int) {
	rc, _ := asynq.GetRetryCount(ctx)
	mr, _ := asynq.GetMaxRetry(ctx)
	return rc, mr
}

// HandleUploadJob processes the "upload:job" task type.
func HandleUploadJob(ctx context.Context, t *asynq.Task) error {
	start := time.Now()

	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=unknown status=failed duration_ms=%d error=%v", duration.Milliseconds(), err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	jobID := payload["job_id"]
	if jobID == "" {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=unknown status=failed duration_ms=%d error=missing job_id", duration.Milliseconds())
		return fmt.Errorf("missing job_id in task payload")
	}

	if db.DB == nil {
		return fmt.Errorf("database connection is not initialized")
	}

	// Increment retry_count on each attempt
	retried, _ := getRetryInfo(ctx)
	_, dbErr := db.DB.ExecContext(ctx, `
		UPDATE upload_jobs
		SET retry_count = $1, updated_at = now()
		WHERE id = $2
	`, retried, jobID)
	if dbErr != nil {
		log.Printf("worker: failed to update retry_count for job %s: %v", jobID, dbErr)
	}

	// 1. Fetch full job details from PostgreSQL
	var status string
	var mimeType string
	var checksum string
	err := db.DB.QueryRowContext(ctx, `
		SELECT status, mime_type, checksum
		FROM upload_jobs
		WHERE id = $1
	`, jobID).Scan(&status, &mimeType, &checksum)
	if err != nil {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=%s status=failed duration_ms=%d error=%v", jobID, duration.Milliseconds(), err)
		return fmt.Errorf("failed to fetch job from DB: %w", err)
	}

	// 2. Checks status != cancelled before doing any work
	if status == "cancelled" {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=%s status=cancelled_skipped duration_ms=%d error=nil", jobID, duration.Milliseconds())
		return nil
	}

	// 3. Checks idempotency: if a file with the same checksum already has a storage_url in PostgreSQL, skip the R2 upload and reuse the existing URL
	var existingStorageURL string
	err = db.DB.QueryRowContext(ctx, `
		SELECT storage_url
		FROM upload_jobs
		WHERE checksum = $1 AND status = 'success' AND storage_url IS NOT NULL
		LIMIT 1
	`, checksum).Scan(&existingStorageURL)
	if err == nil {
		// Found existing URL, reuse it
		_, updateErr := db.DB.ExecContext(ctx, `
			UPDATE upload_jobs
			SET status = 'success', storage_url = $1, updated_at = now()
			WHERE id = $2
		`, existingStorageURL, jobID)
		if updateErr != nil {
			return fmt.Errorf("failed to update job status on idempotency hit: %w", updateErr)
		}
		// Clean up the temp file if exists
		tempFilePath := filepath.Join("tmp/uploads", jobID)
		_ = os.Remove(tempFilePath)

		duration := time.Since(start)
		log.Printf("Processed task (idempotent): job_id=%s status=success duration_ms=%d error=nil", jobID, duration.Milliseconds())
		return nil
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("failed to check idempotency in DB: %w", err)
	}

	// 4. Read file bytes from temporary local directory
	tempFilePath := filepath.Join("tmp/uploads", jobID)
	data, err := os.ReadFile(tempFilePath)
	if err != nil {
		return fmt.Errorf("failed to read temp file: %w", err)
	}
	defer func() {
		// Clean up temp file on completion
		_ = os.Remove(tempFilePath)
	}()

	// 5. Upload file bytes to R2
	r2Client := uploader
	if r2Client == nil {
		r2Client = storage.NewR2Client(config.Load())
	}
	uploadOut, err := r2Client.Upload(ctx, storage.UploadInput{
		Key:      jobID,
		Data:     data,
		MimeType: mimeType,
	})
	if err != nil {
		duration := time.Since(start)
		log.Printf("Processed task: job_id=%s status=failed duration_ms=%d error=%v", jobID, duration.Milliseconds(), err)
		return fmt.Errorf("failed to upload to R2: %w", err)
	}

	// 6. On success: updates upload_job with status: success and storage_url
	_, err = db.DB.ExecContext(ctx, `
		UPDATE upload_jobs
		SET status = 'success', storage_url = $1, updated_at = now()
		WHERE id = $2
	`, uploadOut.URL, jobID)
	if err != nil {
		return fmt.Errorf("failed to update job status on success: %w", err)
	}

	duration := time.Since(start)
	log.Printf("Processed task: job_id=%s status=success duration_ms=%d error=nil", jobID, duration.Milliseconds())
	return nil
}

// CustomRetryDelay returns the delay for retries: 30s -> 5m -> 30m.
func CustomRetryDelay(n int, e error, t *asynq.Task) time.Duration {
	switch n {
	case 1:
		return 30 * time.Second
	case 2:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// CustomErrorHandler handles task failures and updates the database.
func CustomErrorHandler(ctx context.Context, task *asynq.Task, err error) {
	var payload map[string]string
	if unmErr := json.Unmarshal(task.Payload(), &payload); unmErr != nil {
		return
	}
	jobID := payload["job_id"]
	if jobID == "" {
		return
	}

	retried, maxRetry := getRetryInfo(ctx)

	// Check if this was the last retry
	if retried >= maxRetry-1 {
		var ttlHours int = 24
		if db.DB != nil {
			_ = db.DB.QueryRowContext(ctx, `
				SELECT ds.ttl_hours
				FROM upload_jobs uj
				JOIN developer_settings ds ON uj.developer_id = ds.developer_id
				WHERE uj.id = $1
			`, jobID).Scan(&ttlHours)

			expiresAt := time.Now().Add(time.Duration(ttlHours) * time.Hour)
			failureReason := err.Error()

			_, dbErr := db.DB.ExecContext(ctx, `
				UPDATE upload_jobs
				SET status = 'failed', failure_reason = $1, expires_at = $2, updated_at = now()
				WHERE id = $3
			`, failureReason, expiresAt, jobID)
			if dbErr != nil {
				log.Printf("worker error handler: failed to update job %s to failed: %v", jobID, dbErr)
			}
		}
	}
}

// CleanExpiredFailedJobs finds jobs where expires_at < now() and status = failed,
// and deletes them in chunks of 100 with a checkpoint after each chunk.
func CleanExpiredFailedJobs(ctx context.Context, dbConn *sql.DB) error {
	for {
		// Start a transaction for the chunk deletion to have a checkpoint
		tx, err := dbConn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to start transaction: %w", err)
		}

		// Select 100 expired failed jobs
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM upload_jobs
			WHERE status = 'failed' AND expires_at < now()
			LIMIT 100
			FOR UPDATE SKIP LOCKED
		`)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to select expired jobs: %w", err)
		}

		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()

		if len(ids) == 0 {
			tx.Rollback()
			break // No more expired failed jobs to delete
		}

		// Construct query with placeholders
		placeholders := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		query := fmt.Sprintf("DELETE FROM upload_jobs WHERE id IN (%s)", strings.Join(placeholders, ","))
		_, err = tx.ExecContext(ctx, query, args...)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to delete chunk: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit chunk: %w", err)
		}

		log.Printf("worker cron: deleted chunk of %d expired failed jobs", len(ids))

		if len(ids) < 100 {
			break // Last chunk was smaller than 100, so we're done
		}
	}
	return nil
}
