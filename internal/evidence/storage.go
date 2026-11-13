// Package evidence owns exact-version custody. Storage I/O never runs in a DB transaction.
package evidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const PlanMax int64 = 67108864
const SegmentMax int64 = 1048576
const PartSize = 8388608

// ErrUploadIncomplete permits a fresh upload ID only after an exact, matching
// receipt proves that no recoverable version was retained for this ID.
var ErrUploadIncomplete = errors.New("upload incomplete; allocate a new upload ID")

var ErrUnavailable = errors.New("evidence unavailable")
var ErrInvalid = errors.New("invalid evidence input")

type Settings struct{ Bucket, Region, Owner string }

func (s Settings) Validate() error {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") || regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(s.Bucket) || !regexp.MustCompile(`^(us|eu|ap|sa|ca|me|af|il|mx)-(gov-)?[a-z]+-\d$`).MatchString(s.Region) || !regexp.MustCompile(`^\d{12}$`).MatchString(s.Owner) {
		return ErrInvalid
	}
	return nil
}

type Object struct {
	Version    string
	Size       int64
	Encryption string
	Body       io.ReadCloser
}
type Storage interface {
	Check(context.Context) error
	Put(context.Context, string, io.Reader, int64) (string, error)
	Get(context.Context, string, string) (Object, error)
}
type S3 struct {
	client   *s3.Client
	settings Settings
}

// NewS3 uses only the platform EC2 role. Shared profiles, environment credentials
// and endpoint overrides are deliberately outside this constructor.
func NewS3(ctx context.Context, s Settings) (*S3, error) {
	if e := s.Validate(); e != nil {
		return nil, e
	}
	cfg := aws.Config{Region: s.Region, Credentials: aws.NewCredentialsCache(ec2rolecreds.New(func(o *ec2rolecreds.Options) { o.Client = imds.New(imds.Options{Endpoint: "http://169.254.169.254"}) }))}
	c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = nil; o.RetryMaxAttempts = 1 })
	return &S3{c, s}, nil
}
func (s *S3) Check(ctx context.Context) error {
	b, o := aws.String(s.settings.Bucket), aws.String(s.settings.Owner)
	head, e := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: b, ExpectedBucketOwner: o})
	if e != nil || aws.ToString(head.BucketRegion) != s.settings.Region {
		return ErrUnavailable
	}
	v, e := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b, ExpectedBucketOwner: o})
	if e != nil || v.Status != types.BucketVersioningStatusEnabled {
		return ErrUnavailable
	}
	enc, e := s.client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b, ExpectedBucketOwner: o})
	if e != nil || enc.ServerSideEncryptionConfiguration == nil || len(enc.ServerSideEncryptionConfiguration.Rules) != 1 {
		return ErrUnavailable
	}
	rule := enc.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault
	if rule == nil || rule.SSEAlgorithm != types.ServerSideEncryptionAes256 {
		return ErrUnavailable
	}
	p, e := s.client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b, ExpectedBucketOwner: o})
	if e != nil || p.PublicAccessBlockConfiguration == nil {
		return ErrUnavailable
	}
	x := p.PublicAccessBlockConfiguration
	if !aws.ToBool(x.BlockPublicAcls) || !aws.ToBool(x.BlockPublicPolicy) || !aws.ToBool(x.IgnorePublicAcls) || !aws.ToBool(x.RestrictPublicBuckets) {
		return ErrUnavailable
	}
	return nil
}
func validVersion(v string) bool { return v != "" && v != "null" && len(v) <= 1024 }
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) (string, error) {
	b, o, k := aws.String(s.settings.Bucket), aws.String(s.settings.Owner), aws.String(key)
	if size <= PartSize {
		out, e := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: b, ExpectedBucketOwner: o, Key: k, Body: r, ContentLength: aws.Int64(size), ServerSideEncryption: types.ServerSideEncryptionAes256})
		if e != nil || !validVersion(aws.ToString(out.VersionId)) {
			return "", ErrUnavailable
		}
		return *out.VersionId, nil
	}
	m, e := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: b, ExpectedBucketOwner: o, Key: k, ServerSideEncryption: types.ServerSideEncryptionAes256})
	if e != nil {
		return "", ErrUnavailable
	}
	done := false
	defer func() {
		if !done {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5e9)
			defer cancel()
			s.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: b, ExpectedBucketOwner: o, Key: k, UploadId: m.UploadId})
		}
	}()
	parts := []types.CompletedPart{}
	buf := make([]byte, PartSize)
	for n := int32(1); size > 0; n++ {
		want := min(size, int64(len(buf)))
		if _, e = io.ReadFull(r, buf[:want]); e != nil {
			return "", ErrUnavailable
		}
		part, e := s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: b, ExpectedBucketOwner: o, Key: k, UploadId: m.UploadId, PartNumber: aws.Int32(n), ContentLength: aws.Int64(want), Body: bytes.NewReader(buf[:want])})
		if e != nil {
			return "", ErrUnavailable
		}
		parts = append(parts, types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(n)})
		size -= want
	}
	out, e := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: b, ExpectedBucketOwner: o, Key: k, UploadId: m.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if e != nil || !validVersion(aws.ToString(out.VersionId)) {
		return "", ErrUnavailable
	}
	done = true
	return *out.VersionId, nil
}
func (s *S3) Get(ctx context.Context, key, version string) (Object, error) {
	if !validVersion(version) {
		return Object{}, ErrUnavailable
	}
	out, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.settings.Bucket), ExpectedBucketOwner: aws.String(s.settings.Owner), Key: aws.String(key), VersionId: aws.String(version)})
	if e != nil {
		var api smithy.APIError
		if errors.As(e, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NoSuchVersion" || api.ErrorCode() == "NotFound") {
			return Object{}, ErrInvalid
		}
		return Object{}, ErrUnavailable
	}
	return Object{aws.ToString(out.VersionId), aws.ToInt64(out.ContentLength), string(out.ServerSideEncryption), out.Body}, nil
}

// spool bounds memory and rejects incomplete/extra bytes before custody. The
// owner-only temporary file is removed by its caller, including every failure.
func spool(r io.Reader, size int64, digest string) (*os.File, error) {
	f, e := os.CreateTemp("", "cosmoformer-evidence-*")
	if e != nil {
		return nil, ErrUnavailable
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	if e != nil || n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return nil, ErrInvalid
	}
	if _, e = f.Seek(0, 0); e != nil {
		return nil, e
	}
	ok = true
	return f, nil
}

// Delete never creates a delete marker or addresses an unspecified version.
func (s *S3) Delete(ctx context.Context, key, version string) error {
	if key == "" || !validVersion(version) {
		return ErrInvalid
	}
	out, e := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.settings.Bucket), ExpectedBucketOwner: aws.String(s.settings.Owner), Key: aws.String(key), VersionId: aws.String(version)})
	if e != nil {
		return ErrUnavailable
	}
	// A successful exact-version DELETE is idempotent. S3 may omit optional
	// response version metadata; the request identity remains mandatory.
	if got := aws.ToString(out.VersionId); got != "" && got != version || aws.ToBool(out.DeleteMarker) {
		return ErrInvalid
	}
	return nil
}
