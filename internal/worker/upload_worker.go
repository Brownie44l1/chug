package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
