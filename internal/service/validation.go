package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

var (
	ErrInvalidFileType  = errors.New("INVALID_FILE_TYPE")
	ErrFileTooLarge     = errors.New("FILE_TOO_LARGE")
	ErrChecksumMismatch = errors.New("CHECKSUM_MISMATCH")
)

type ValidationResult struct {
	MimeType string
	Size     int64
	Checksum string
	Bytes    []byte
}

// ValidateFile performs checks on the uploaded file stream.
// It verifies the magic bytes to detect file type, checks that size does not exceed maxSizeBytes
// without reading the whole file into memory if it is oversized, and verifies the SHA-256 checksum.
func ValidateFile(reader io.Reader, maxSizeBytes int64, providedChecksum string) (*ValidationResult, error) {
	// Create a limited reader to read at most maxSizeBytes + 1 bytes.
	// This prevents memory exhaustion on extremely large uploads.
	lr := io.LimitReader(reader, maxSizeBytes+1)

	// Read the first 12 bytes to determine magic bytes (WEBP requires 12 bytes).
	header := make([]byte, 12)
	n, err := io.ReadFull(lr, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}

	// Detect MIME type based on magic bytes
	mimeType, ok := DetectMimeType(header[:n])
	if !ok {
		return nil, ErrInvalidFileType
	}

	// Read the rest of the stream (up to the limit of maxSizeBytes + 1)
	restBytes, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}

	totalSize := int64(n) + int64(len(restBytes))
	if totalSize > maxSizeBytes {
		return nil, ErrFileTooLarge
	}

	// Combine header and rest to get full bytes
	fullBytes := make([]byte, totalSize)
	copy(fullBytes[:n], header[:n])
	copy(fullBytes[n:], restBytes)

	// Compute SHA-256 checksum
	hasher := sha256.New()
	hasher.Write(fullBytes)
	computedChecksum := hex.EncodeToString(hasher.Sum(nil))

	// Verify checksum if provided
	if providedChecksum != "" && computedChecksum != providedChecksum {
		return nil, ErrChecksumMismatch
	}

	return &ValidationResult{
		MimeType: mimeType,
		Size:     totalSize,
		Checksum: computedChecksum,
		Bytes:    fullBytes,
	}, nil
}

// DetectMimeType detects MIME type using magic bytes at the beginning of the file.
func DetectMimeType(data []byte) (string, bool) {
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg", true
	}
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png", true
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return "image/gif", true
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", true
	}
	return "", false
}
