package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Brownie44l1/chug/internal/queue"
	"github.com/Brownie44l1/chug/internal/service"
)

type UploadHandler struct {
	db *sql.DB
}

func NewUploadHandler(database *sql.DB) *UploadHandler {
	return &UploadHandler{db: database}
}

func (h *UploadHandler) Create(c *gin.Context) {
	// 1. Retrieve developer context
	developerIDRaw, exists := c.Get("developer_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing developer context"})
		return
	}
	developerID := developerIDRaw.(string)

	apiKeyIDRaw, exists := c.Get("api_key_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing API key context"})
		return
	}
	apiKeyID := apiKeyIDRaw.(string)

	// 2. Fetch developer settings (max_file_size_bytes, max_retries, ttl_hours)
	var maxFileSizeBytes int64 = 10485760 // default 10MB
	var maxRetries int = 3                // default 3
	var ttlHours int = 24                 // default 24 hours

	err := h.db.QueryRow(`
		SELECT max_file_size_bytes, max_retries, ttl_hours
		FROM developer_settings
		WHERE developer_id = $1
	`, developerID).Scan(&maxFileSizeBytes, &maxRetries, &ttlHours)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("upload handler: failed to fetch developer settings: %v", err)
	}

	// 3. Extract parameters from multipart form
	providedChecksum := c.PostForm("checksum")

	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing file in multipart form"})
		return
	}
	defer file.Close()

	// 4. Validate the file (magic bytes, size limits, checksums)
	valRes, err := service.ValidateFile(file, maxFileSizeBytes, providedChecksum)
	if err != nil {
		if errors.Is(err, service.ErrInvalidFileType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_FILE_TYPE"})
			return
		}
		if errors.Is(err, service.ErrFileTooLarge) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "FILE_TOO_LARGE"})
			return
		}
		if errors.Is(err, service.ErrChecksumMismatch) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "CHECKSUM_MISMATCH"})
			return
		}
		log.Printf("upload handler: file validation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error during file validation"})
		return
	}

	// 5. Insert pending upload job record in PostgreSQL
	var jobID string
	err = h.db.QueryRow(`
		INSERT INTO upload_jobs (developer_id, api_key_id, status, file_name, file_size_bytes, mime_type, checksum, max_retries)
		VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7)
		RETURNING id
	`, developerID, apiKeyID, fileHeader.Filename, valRes.Size, valRes.MimeType, valRes.Checksum, maxRetries).Scan(&jobID)

	if err != nil {
		log.Printf("upload handler: failed to insert job record: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload job record"})
		return
	}

	// 6. Write file bytes to temporary local directory (ignored by git)
	tempDir := "tmp/uploads"
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Printf("upload handler: failed to create temp directory: %v", err)
		h.markJobAsFailed(jobID, "Failed to create temp directory on server", ttlHours)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file temporarily"})
		return
	}

	tempFilePath := filepath.Join(tempDir, jobID)
	if err := os.WriteFile(tempFilePath, valRes.Bytes, 0644); err != nil {
		log.Printf("upload handler: failed to write file to temp path: %v", err)
		h.markJobAsFailed(jobID, "Failed to write file to temp storage", ttlHours)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file temporarily"})
		return
	}

	// 7. Enqueue the job to the Redis queue
	if err := queue.EnqueueJob(jobID, maxRetries); err != nil {
		log.Printf("upload handler: failed to enqueue job %s: %v", jobID, err)
		
		// Clean up the temp file
		_ = os.Remove(tempFilePath)

		// Mark the job as failed in the DB
		h.markJobAsFailed(jobID, "Failed to enqueue job: "+err.Error(), ttlHours)

		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to queue upload job"})
		return
	}

	// 8. Respond with 202 Accepted and job ID
	c.JSON(http.StatusAccepted, gin.H{"job_id": jobID})
}

// Get handles retrieving the status and details of an upload job.
func (h *UploadHandler) Get(c *gin.Context) {
	// 1. Retrieve developer context
	developerIDRaw, exists := c.Get("developer_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing developer context"})
		return
	}
	developerID := developerIDRaw.(string)

	jobID := c.Param("job_id")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing job ID"})
		return
	}

	// 2. Fetch job from DB
	var job struct {
		ID            string
		DeveloperID   string
		Status        string
		StorageURL    sql.NullString
		FailureReason sql.NullString
		ExpiresAt     sql.NullTime
	}

	err := h.db.QueryRow(`
		SELECT id, developer_id, status, storage_url, failure_reason, expires_at
		FROM upload_jobs
		WHERE id = $1
	`, jobID).Scan(&job.ID, &job.DeveloperID, &job.Status, &job.StorageURL, &job.FailureReason, &job.ExpiresAt)

	if err != nil {
		if err == sql.ErrNoRows || strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
			return
		}
		log.Printf("get job handler: failed to fetch job: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	// 3. Verify developer ownership
	if job.DeveloperID != developerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: you do not own this job"})
		return
	}

	// 4. Build consistent response shape (null if missing)
	var storageURL *string
	if job.StorageURL.Valid {
		storageURL = &job.StorageURL.String
	}

	var failureReason *string
	if job.FailureReason.Valid {
		failureReason = &job.FailureReason.String
	}

	var expiresAt *time.Time
	if job.ExpiresAt.Valid {
		expiresAt = &job.ExpiresAt.Time
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":         job.ID,
		"status":         job.Status,
		"storage_url":    storageURL,
		"failure_reason": failureReason,
		"expires_at":     expiresAt,
	})
}

// markJobAsFailed updates a job status to failed and sets its expiration TTL.
func (h *UploadHandler) markJobAsFailed(jobID string, reason string, ttlHours int) {
	expiresAt := time.Now().Add(time.Duration(ttlHours) * time.Hour)
	_, err := h.db.Exec(`
		UPDATE upload_jobs
		SET status = 'failed', failure_reason = $1, expires_at = $2, updated_at = now()
		WHERE id = $3
	`, reason, expiresAt, jobID)
	if err != nil {
		log.Printf("upload handler helper: failed to mark job %s as failed: %v", jobID, err)
	}
}

// Retry handles manually retrying a failed job within its TTL window.
func (h *UploadHandler) Retry(c *gin.Context) {
	// 1. Retrieve developer context
	developerIDRaw, exists := c.Get("developer_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing developer context"})
		return
	}
	developerID := developerIDRaw.(string)

	jobID := c.Param("job_id")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing job ID"})
		return
	}

	// 2. Fetch job from DB
	var job struct {
		DeveloperID string
		Status      string
		ExpiresAt   sql.NullTime
		MaxRetries  int
	}

	err := h.db.QueryRow(`
		SELECT developer_id, status, expires_at, max_retries
		FROM upload_jobs
		WHERE id = $1
	`, jobID).Scan(&job.DeveloperID, &job.Status, &job.ExpiresAt, &job.MaxRetries)

	if err != nil {
		if err == sql.ErrNoRows || strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
			return
		}
		log.Printf("retry job handler: failed to fetch job: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	// 3. Verify developer ownership
	if job.DeveloperID != developerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: you do not own this job"})
		return
	}

	// 4. Returns 404 Not Found if expires_at has passed
	if job.ExpiresAt.Valid && job.ExpiresAt.Time.Before(time.Now()) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job not found or has expired"})
		return
	}

	// 5. Returns 400 Bad Request if status is not failed
	if job.Status != "failed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job is not in a failed state"})
		return
	}

	// 6. Reset job record properties in PostgreSQL
	_, err = h.db.Exec(`
		UPDATE upload_jobs
		SET status = 'pending', retry_count = 0, failure_reason = NULL, expires_at = NULL, updated_at = now()
		WHERE id = $1
	`, jobID)
	if err != nil {
		log.Printf("retry job handler: failed to reset job properties: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reset job properties"})
		return
	}

	// 7. Re-enqueue the job to the background worker
	if err := queue.EnqueueJob(jobID, job.MaxRetries); err != nil {
		log.Printf("retry job handler: failed to re-enqueue job %s: %v", jobID, err)
		// Mark it back as failed since enqueueing failed
		h.markJobAsFailed(jobID, "Failed to re-enqueue job: "+err.Error(), 24)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to queue upload job"})
		return
	}

	// 8. Return 202 Accepted
	c.JSON(http.StatusAccepted, gin.H{"job_id": jobID})
}
