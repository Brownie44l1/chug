package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateFile(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0x00, 0x01, 0x02}
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	gifBytes := []byte("GIF89aSomeImageDataHere")
	webpBytes := []byte("RIFF1234WEBPSomeImageData")

	getChecksum := func(b []byte) string {
		h := sha256.New()
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}

	t.Run("Valid JPEG file", func(t *testing.T) {
		reader := bytes.NewReader(jpegBytes)
		checksum := getChecksum(jpegBytes)
		res, err := ValidateFile(reader, 100, checksum)

		assert.NoError(t, err)
		assert.Equal(t, "image/jpeg", res.MimeType)
		assert.Equal(t, int64(len(jpegBytes)), res.Size)
		assert.Equal(t, checksum, res.Checksum)
		assert.Equal(t, jpegBytes, res.Bytes)
	})

	t.Run("Valid PNG file", func(t *testing.T) {
		reader := bytes.NewReader(pngBytes)
		checksum := getChecksum(pngBytes)
		res, err := ValidateFile(reader, 100, checksum)

		assert.NoError(t, err)
		assert.Equal(t, "image/png", res.MimeType)
		assert.Equal(t, pngBytes, res.Bytes)
	})

	t.Run("Valid GIF file", func(t *testing.T) {
		reader := bytes.NewReader(gifBytes)
		checksum := getChecksum(gifBytes)
		res, err := ValidateFile(reader, 100, checksum)

		assert.NoError(t, err)
		assert.Equal(t, "image/gif", res.MimeType)
		assert.Equal(t, gifBytes, res.Bytes)
	})

	t.Run("Valid WEBP file", func(t *testing.T) {
		reader := bytes.NewReader(webpBytes)
		checksum := getChecksum(webpBytes)
		res, err := ValidateFile(reader, 100, checksum)

		assert.NoError(t, err)
		assert.Equal(t, "image/webp", res.MimeType)
		assert.Equal(t, webpBytes, res.Bytes)
	})

	t.Run("Invalid File Type (txt)", func(t *testing.T) {
		txtBytes := []byte("This is a simple text file that is not an image.")
		reader := bytes.NewReader(txtBytes)
		_, err := ValidateFile(reader, 100, "")

		assert.ErrorIs(t, err, ErrInvalidFileType)
	})

	t.Run("File Too Large", func(t *testing.T) {
		reader := bytes.NewReader(pngBytes)
		// Max size set to 5 bytes, PNG is 8 bytes
		_, err := ValidateFile(reader, 5, "")

		assert.ErrorIs(t, err, ErrFileTooLarge)
	})

	t.Run("Checksum Mismatch", func(t *testing.T) {
		reader := bytes.NewReader(pngBytes)
		_, err := ValidateFile(reader, 100, "wrongchecksum")

		assert.ErrorIs(t, err, ErrChecksumMismatch)
	})
}
