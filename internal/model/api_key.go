package model

import "time"

type APIKey struct {
	ID          string     `json:"id" db:"id"`
	DeveloperID string     `json:"developer_id" db:"developer_id"`
	KeyHash     string     `json:"-" db:"key_hash"`
	Label       string     `json:"label" db:"label"`
	IsActive    bool       `json:"is_active" db:"is_active"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty" db:"revoked_at"`
}
