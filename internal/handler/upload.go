package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	if err := queue.EnqueueJob(jobID); err != nil {
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
