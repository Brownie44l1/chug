package model

import "time"

type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusProcessing JobStatus = "processing"
	StatusSuccess    JobStatus = "success"
	StatusFailed     JobStatus = "failed"
	StatusCancelled  JobStatus = "cancelled"
)

type UploadJob struct {
	ID            string     `json:"id" db:"id"`
	DeveloperID   string     `json:"developer_id" db:"developer_id"`
	APIKeyID      string     `json:"api_key_id" db:"api_key_id"`
	Status        JobStatus  `json:"status" db:"status"`
	FileName      string     `json:"file_name" db:"file_name"`
	FileSizeBytes int64      `json:"file_size_bytes" db:"file_size_bytes"`
	MimeType      string     `json:"mime_type" db:"mime_type"`
	Checksum      string     `json:"checksum" db:"checksum"`
	StorageURL    *string    `json:"storage_url" db:"storage_url"`
	FailureReason *string    `json:"failure_reason" db:"failure_reason"`
	RetryCount    int        `json:"retry_count" db:"retry_count"`
	MaxRetries    int        `json:"max_retries" db:"max_retries"`
	ExpiresAt     *time.Time `json:"expires_at" db:"expires_at"`
	CreatedAt     time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at" db:"updated_at"`
}
