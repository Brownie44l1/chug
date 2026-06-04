package storage

import (
	"bytes"
	"context"
	"fmt"
	"mime"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/Brownie44l1/chug/internal/config"
)

type R2Client struct {
	client     *s3.Client
	bucketName string
	publicURL  string
}

func NewR2Client(cfg *config.Config) *R2Client {
	r2Resolver := aws.EndpointResolverWithOptionsFunc(
		func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				URL: fmt.Sprintf(
					"https://%s.r2.cloudflarestorage.com",
					cfg.R2AccountID,
				),
			}, nil
		},
	)

	awsCfg := aws.Config{
		Region: "auto",
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.R2AccessKeyID,
			cfg.R2AccessKeySecret,
			"",
		),
		EndpointResolverWithOptions: r2Resolver,
	}

	client := s3.NewFromConfig(awsCfg)

	return &R2Client{
		client:     client,
		bucketName: cfg.R2BucketName,
		publicURL:  cfg.R2PublicURL,
	}
}

type UploadInput struct {
	Key      string
	Data     []byte
	MimeType string
}

type UploadOutput struct {
	URL string
}

func (r *R2Client) Upload(ctx context.Context, input UploadInput) (*UploadOutput, error) {
	contentType := input.MimeType
	if contentType == "" {
		contentType = mime.TypeByExtension(".bin")
	}

	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(r.bucketName),
		Key:         aws.String(input.Key),
		Body:        bytes.NewReader(input.Data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return nil, fmt.Errorf("r2 upload failed: %w", err)
	}

	url := fmt.Sprintf("%s/%s", r.publicURL, input.Key)

	return &UploadOutput{URL: url}, nil
}

func (r *R2Client) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("r2 delete failed: %w", err)
	}
	return nil
}