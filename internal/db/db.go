package db

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"

	_ "github.com/lib/pq"
)

var DB *sql.DB

// Connect opens a connection to the PostgreSQL database and pings it to verify availability.
func Connect(postgresURL string) (*sql.DB, error) {
	if postgresURL == "" {
		return nil, fmt.Errorf("POSTGRES_URL environment variable is empty")
	}

	db, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	DB = db
	log.Println("successfully connected to PostgreSQL database")
	return db, nil
}

// Close closes the database connection.
func Close() error {
	if DB != nil {
		return DB.Close()
	}
	return nil
}

// SeedDefaultDeveloperAndKey seeds a default developer and API key in PostgreSQL if it doesn't already exist.
func SeedDefaultDeveloperAndKey(defaultAPIKey, hashSecret string) error {
	if defaultAPIKey == "" {
		log.Println("no default API key specified in environment, skipping seeding")
		return nil
	} 

	// Compute key hash using HMAC-SHA256
	h := hmac.New(sha256.New, []byte(hashSecret))
	h.Write([]byte(defaultAPIKey))
	hashedKey := hex.EncodeToString(h.Sum(nil))

	// Check if this API key hash already exists
	var exists bool
	err := DB.QueryRow("SELECT EXISTS(SELECT 1 FROM api_keys WHERE key_hash = $1)", hashedKey).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check if default key exists: %w", err)
	}

	if exists {
		log.Println("default API key already seeded")
		return nil
	}

	// Start transaction
	tx, err := DB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert developer or get existing
	var developerID string
	err = tx.QueryRow(`
		INSERT INTO developers (email, hashed_password)
		VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, "default-developer@chug.dev", "dummy-hashed-password").Scan(&developerID)
	if err != nil {
		return fmt.Errorf("failed to insert default developer: %w", err)
	}

	// Insert API key
	var apiKeyID string
	err = tx.QueryRow(`
		INSERT INTO api_keys (developer_id, key_hash, label, is_active)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, developerID, hashedKey, "default-demo-key", true).Scan(&apiKeyID)
	if err != nil {
		return fmt.Errorf("failed to insert default API key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("successfully seeded default developer (id: %s) and API key (id: %s)", developerID, apiKeyID)
	return nil
}
