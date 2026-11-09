package awsaccess

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/logging"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// All endpoint injection stays package-private. Production never loads SDK
// config, profiles, environment endpoints/credentials, proxies or a global
// default transport. Constructors perform no I/O.
type clients struct {
	metadata *imds.Client
	sts      func(aws.Credentials, string) *sts.Client
	s3       func(aws.Credentials, string) *s3.Client
}

func productionClients() clients {
	h := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
	}}
	return clients{
		metadata: imds.New(imds.Options{Endpoint: "http://169.254.169.254", HTTPClient: h, ClientEnableState: imds.ClientEnabled, EnableFallback: aws.FalseTernary, Retryer: aws.NopRetryer{}, Logger: logging.Nop{}}),
		sts: func(c aws.Credentials, region string) *sts.Client {
			return sts.NewFromConfig(fixedConfig(c, region, h))
		},
		s3: func(c aws.Credentials, region string) *s3.Client {
			return s3.NewFromConfig(fixedConfig(c, region, h), func(o *s3.Options) { o.DisableS3ExpressSessionAuth = aws.Bool(true) })
		},
	}
}

func fixedConfig(c aws.Credentials, region string, h *http.Client) aws.Config {
	return aws.Config{Region: region, Credentials: credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken), HTTPClient: h, Retryer: func() aws.Retryer { return aws.NopRetryer{} }, Logger: logging.Nop{}}
}

func (c clients) bootstrap(ctx context.Context, expected role) (aws.Credentials, error) {
	read := func(path string, limit int64) ([]byte, error) {
		out, e := c.metadata.GetMetadata(ctx, &imds.GetMetadataInput{Path: path})
		if e != nil {
			return nil, ErrAccess
		}
		defer out.Content.Close()
		b, e := io.ReadAll(io.LimitReader(out.Content, limit+1))
		if e != nil || int64(len(b)) > limit {
			return nil, ErrAccess
		}
		return b, nil
	}
	// The SDK EC2 credential provider picks the first listed role and supports
	// expiry extension. Read explicitly instead: ambiguity and renewal are denied.
	list, e := read("iam/security-credentials/", 256)
	if e != nil || strings.TrimSuffix(string(list), "\n") != expected.name {
		return aws.Credentials{}, ErrAccess
	}
	b, e := read("iam/security-credentials/"+expected.name, 32768)
	if e != nil {
		return aws.Credentials{}, ErrAccess
	}
	var v struct {
		Code        string    `json:"Code"`
		LastUpdated time.Time `json:"LastUpdated"`
		Type        string    `json:"Type"`
		AccessKeyID string    `json:"AccessKeyId"`
		Secret      string    `json:"SecretAccessKey"`
		Token       string    `json:"Token"`
		Expiration  time.Time `json:"Expiration"`
	}
	if runs.Decode(b, &v) != nil || v.Code != "Success" || v.Type != "AWS-HMAC" {
		return aws.Credentials{}, ErrAccess
	}
	return aws.Credentials{AccessKeyID: v.AccessKeyID, SecretAccessKey: v.Secret, SessionToken: v.Token, CanExpire: true, Expires: v.Expiration}, nil
}
