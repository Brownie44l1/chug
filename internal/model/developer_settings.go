package model

import "time"

type DeveloperSettings struct {
	ID                 string    `json:"id" db:"id"`
	DeveloperID        string    `json:"developer_id" db:"developer_id"`
	MaxFileSizeBytes   int64     `json:"max_file_size_bytes" db:"max_file_size_bytes"`
	MaxRetries         int       `json:"max_retries" db:"max_retries"`
	TTLHours           int       `json:"ttl_hours" db:"ttl_hours"`
	MonthlyUploadLimit int       `json:"monthly_upload_limit" db:"monthly_upload_limit"`
	CreatedAt          time.Time `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time `json:"updated_at" db:"updated_at"`
}
