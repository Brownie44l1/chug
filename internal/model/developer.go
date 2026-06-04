package model

import "time"

type Developer struct {
	ID             string     `json:"id" db:"id"`
	Email          string     `json:"email" db:"email"`
	HashedPassword string     `json:"-" db:"hashed_password"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}
