package handler

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// WebhookEndpoint represents a webhook registration.
type WebhookEndpoint struct {
	ID          string    `json:"id"`
	DeveloperID string    `json:"developer_id"`
	URL         string    `json:"url"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
}

// WebhookHandler handles HTTP requests for webhook registration and management.
type WebhookHandler struct {
	db *sql.DB
}

// NewWebhookHandler creates a new WebhookHandler.
func NewWebhookHandler(db *sql.DB) *WebhookHandler {
	return &WebhookHandler{db: db}
}

// RegisterInput represents the input for registering a webhook.
type RegisterInput struct {
	URL string `json:"url" binding:"required"`
}

// Register registers a new webhook endpoint.
func (h *WebhookHandler) Register(c *gin.Context) {
	// 1. Retrieve developer context
	developerIDRaw, exists := c.Get("developer_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing developer context"})
		return
	}
	developerID := developerIDRaw.(string)

	// 2. Bind and validate input
	var input RegisterInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: 'url' is required"})
		return
	}

	// 3. Validate URL is a valid HTTPS address
	parsedURL, err := url.ParseRequestURI(input.URL)
	if err != nil || parsedURL.Scheme != "https" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL: must be a valid HTTPS address"})
		return
	}

	// 4. Generate random HMAC signing secret (32 bytes = 64 characters hex string)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: failed to generate secret"})
		return
	}
	secret := hex.EncodeToString(secretBytes)

	// 5. Insert webhook endpoint into the database
	var endpoint WebhookEndpoint
	err = h.db.QueryRow(`
		INSERT INTO webhook_endpoints (developer_id, url, secret)
		VALUES ($1, $2, $3)
		RETURNING id, developer_id, url, is_active, created_at
	`, developerID, input.URL, secret).Scan(&endpoint.ID, &endpoint.DeveloperID, &endpoint.URL, &endpoint.IsActive, &endpoint.CreatedAt)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: failed to register webhook"})
		return
	}

	// 6. Return response containing the secret exactly once
	c.JSON(http.StatusCreated, gin.H{
		"id":           endpoint.ID,
		"developer_id": endpoint.DeveloperID,
		"url":          endpoint.URL,
		"secret":       secret,
		"is_active":    endpoint.IsActive,
		"created_at":   endpoint.CreatedAt,
	})
}

// List returns all registered endpoints for the developer.
func (h *WebhookHandler) List(c *gin.Context) {
	// 1. Retrieve developer context
	developerIDRaw, exists := c.Get("developer_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing developer context"})
		return
	}
	developerID := developerIDRaw.(string)

	// 2. Query all active webhook endpoints for the developer
	rows, err := h.db.Query(`
		SELECT id, developer_id, url, is_active, created_at
		FROM webhook_endpoints
		WHERE developer_id = $1 AND is_active = true
		ORDER BY created_at DESC
	`, developerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: failed to retrieve webhooks"})
		return
	}
	defer rows.Close()

	endpoints := []WebhookEndpoint{}
	for rows.Next() {
		var ep WebhookEndpoint
		if err := rows.Scan(&ep.ID, &ep.DeveloperID, &ep.URL, &ep.IsActive, &ep.CreatedAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: failed to scan webhooks"})
			return
		}
		endpoints = append(endpoints, ep)
	}

	// 3. Return registered endpoints (secret is not included in WebhookEndpoint json tags)
	c.JSON(http.StatusOK, endpoints)
}

// Delete soft-deletes a webhook endpoint by setting is_active = false.
func (h *WebhookHandler) Delete(c *gin.Context) {
	// 1. Retrieve developer context
	developerIDRaw, exists := c.Get("developer_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing developer context"})
		return
	}
	developerID := developerIDRaw.(string)

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing webhook ID"})
		return
	}

	// 2. Fetch the endpoint to verify existence and developer ownership
	var dbDevID string
	var isActive bool
	err := h.db.QueryRow(`
		SELECT developer_id, is_active
		FROM webhook_endpoints
		WHERE id = $1
	`, id).Scan(&dbDevID, &isActive)

	if err != nil {
		if err == sql.ErrNoRows || strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			c.JSON(http.StatusNotFound, gin.H{"error": "Webhook endpoint not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	// 3. Verify developer ownership
	if dbDevID != developerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: you do not own this webhook endpoint"})
		return
	}

	// If already inactive, we can just return StatusOK (or StatusNotFound, let's look at standard REST. StatusOK is safe)
	if !isActive {
		c.JSON(http.StatusOK, gin.H{"message": "Webhook endpoint deleted successfully"})
		return
	}

	// 4. Update the record to set is_active = false
	_, err = h.db.Exec(`
		UPDATE webhook_endpoints
		SET is_active = false
		WHERE id = $1
	`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: failed to delete webhook"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Webhook endpoint deleted successfully"})
}
