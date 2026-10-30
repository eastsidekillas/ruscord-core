// Package blob stores files (avatars, guild icons) in S3-compatible object
// storage and collects the ones nothing refers to any more
// (docs/adr/0019-files.md).
//
// Keys are content-addressed and never reused for other content, so an
// object is immutable once written and may be cached forever.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var ErrNotFound = errors.New("blob: not found")

// Store is object storage. Implementations: S3 for real deployments,
// Memory for tests.
type Store interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	// Get returns the object's content; ErrNotFound if it does not exist.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes an object; deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// Ping checks that the store is reachable and the bucket exists.
	Ping(ctx context.Context) error
}

// S3Config addresses a bucket on an S3-compatible server.
type S3Config struct {
	Endpoint  string // e.g. http://127.0.0.1:8333 (SeaweedFS)
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
}

// S3 is a Store on an S3-compatible server. It uses path-style addressing
// (endpoint/bucket/key), which SeaweedFS and most self-hosted servers need.
type S3 struct {
	c      *s3.Client
	bucket string
}

func NewS3(cfg S3Config) *S3 {
	c := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	})
	return &S3{c: c, bucket: cfg.Bucket}
}

func (s *S3) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	return nil
}

// maxGet bounds what Get reads into memory; stored files are small images.
const maxGet = 4 << 20

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("blob: get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxGet+1))
	if err != nil {
		return nil, fmt.Errorf("blob: read %s: %w", key, err)
	}
	if len(b) > maxGet {
		return nil, fmt.Errorf("blob: %s is larger than %d bytes", key, maxGet)
	}
	return b, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}

func (s *S3) Ping(ctx context.Context) error {
	if _, err := s.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("blob: bucket %s: %w", s.bucket, err)
	}
	return nil
}

// EnsureBucket creates the bucket if it does not exist. Used in dev; in
// production the bucket is created with the storage, with its own policy.
func (s *S3) EnsureBucket(ctx context.Context) error {
	if err := s.Ping(ctx); err == nil {
		return nil
	}
	_, err := s.c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &owned) {
		return fmt.Errorf("blob: create bucket %s: %w", s.bucket, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var ae smithy.APIError
	return errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound")
}

// Memory is a Store in memory, for tests.
type Memory struct {
	mu   sync.Mutex
	objs map[string][]byte
	// failDelete makes Delete fail, to test the collector's retries.
	failDelete bool
}

// SetFailDelete makes Delete fail (true) or work again, for tests.
func (m *Memory) SetFailDelete(fail bool) {
	m.mu.Lock()
	m.failDelete = fail
	m.mu.Unlock()
}

func NewMemory() *Memory { return &Memory{objs: map[string][]byte{}} }

func (m *Memory) Put(_ context.Context, key string, body []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = bytes.Clone(body)
	return nil
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(b), nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete {
		return errors.New("blob: delete failed (test)")
	}
	delete(m.objs, key)
	return nil
}

func (m *Memory) Ping(context.Context) error { return nil }

// Keys lists stored keys, for tests.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objs))
	for k := range m.objs {
		out = append(out, k)
	}
	return out
}
