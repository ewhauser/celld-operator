// Package recovery provides narrowly scoped read-only primary S3 evidence.
package recovery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

// ObjectAPI deliberately excludes mutation, application reads and discovery APIs.
type ObjectAPI interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}
type S3Reader struct {
	API    ObjectAPI
	Bucket string
}

var nodeKey = regexp.MustCompile(`^nodes/[A-Za-z0-9_.-]{1,128}\.json$`)

// AWSReader uses the operator's externally provisioned identity (IRSA or EKS Pod
// Identity), never the fleet's writing identity. Disable implicit node-role and
// shared-file fallbacks and endpoint overrides. No credential lookup occurs here.
func AWSReader(ctx context.Context, bucket, region string) (*S3Reader, error) {
	api, err := awsClient(ctx, region)
	if err != nil {
		return nil, err
	}
	return &S3Reader{API: api, Bucket: bucket}, nil
}

// awsClient holds every hardening AWSReader documents: the region is pinned,
// shared config/credential files and IMDS are disabled, retries are bounded at
// 2, the HTTP client times out at 3s and refuses redirects, and no endpoint
// override, acceleration, dual-stack or ARN region is honored. The client is
// bucket-independent; S3Reader supplies the bucket per read.
func awsClient(ctx context.Context, region string) (*s3.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region),
		config.WithSharedConfigFiles([]string{}), config.WithSharedCredentialsFiles([]string{}),
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled), config.WithRetryMaxAttempts(2),
		config.WithHTTPClient(&http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = nil
		o.EndpointResolverV2 = s3.NewDefaultEndpointResolverV2()
		o.UseAccelerate = false
		o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateDisabled
		o.UseARNRegion = false
	}), nil
}

// ClientCache reuses one hardened client per region across reconciles. Building
// a client runs config.LoadDefaultConfig, which creates a fresh credential
// cache, so an uncached construction costs a full AssumeRoleWithWebIdentity (or
// Pod Identity agent call) plus new TLS connections on every reconcile pass.
// The SDK's own credential cache refreshes and expires credentials; nothing here
// touches credentials. Fleets are few and regions fewer, so entries are never
// evicted: the map holds at most one client per region the operator has served.
type ClientCache struct {
	mu        sync.Mutex
	clients   map[string]*s3.Client
	newClient func(context.Context, string) (*s3.Client, error)
}

func NewClientCache() *ClientCache { return &ClientCache{newClient: awsClient} }

// Reader returns a reader for one bucket over the region's shared client. The
// S3Reader wrapper is cheap and per-bucket, so two fleets in the same region
// with different buckets share connections without sharing a bucket.
func (c *ClientCache) Reader(bucket, region string) (*S3Reader, error) {
	api, err := c.client(region)
	if err != nil {
		return nil, err
	}
	return &S3Reader{API: api, Bucket: bucket}, nil
}
func (c *ClientCache) client(region string) (*s3.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if api, ok := c.clients[region]; ok {
		return api, nil
	}
	// Construct from a context detached from the caller's. LoadDefaultConfig uses
	// its context only while constructing; the built client carries none of it.
	// But a reconcile context canceled mid-construction would fail a client that
	// every later reconcile then shares, so the cached client must never be born
	// from a context that a single reconcile can cancel. awsClient bounds this
	// with its own 5s timeout. Construction is serialized under the lock, which
	// also keeps four concurrent reconciles from issuing four STS calls.
	api, err := c.newClient(context.Background(), region)
	if err != nil {
		// Never cache a failure: the next reconcile retries construction.
		return nil, err
	}
	if c.clients == nil {
		c.clients = map[string]*s3.Client{}
	}
	c.clients[region] = api
	return api, nil
}
func (r *S3Reader) Get(ctx context.Context, key string) ([]byte, error) {
	if !nodeKey.MatchString(key) || key == "nodes/..json" || key == "nodes/...json" {
		return nil, errors.New("read outside node metadata scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := r.API.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	if out == nil || out.Body == nil {
		return nil, errors.New("missing object body")
	}
	data, readErr := io.ReadAll(io.LimitReader(out.Body, (1<<20)+1))
	closeErr := out.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > 1<<20 || (out.ContentLength != nil && *out.ContentLength != int64(len(data))) {
		return nil, errors.New("partial or oversized metadata")
	}
	return data, nil
}
func (r *S3Reader) List(ctx context.Context, prefix, continuation string) (v050.Page, error) {
	if prefix != "nodes/" && prefix != "log/" {
		return v050.Page{}, errors.New("list outside recovery scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	in := &s3.ListObjectsV2Input{Bucket: aws.String(r.Bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1000)}
	if continuation != "" {
		in.ContinuationToken = aws.String(continuation)
	}
	out, err := r.API.ListObjectsV2(ctx, in)
	if err != nil {
		return v050.Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return v050.Page{}, err
	}
	if out == nil || out.IsTruncated == nil || len(out.CommonPrefixes) != 0 {
		return v050.Page{}, errors.New("ambiguous listing")
	}
	page := v050.Page{Complete: !*out.IsTruncated, Next: aws.ToString(out.NextContinuationToken)}
	if (page.Complete && page.Next != "") || (!page.Complete && (page.Next == "" || page.Next == continuation)) {
		return v050.Page{}, errors.New("invalid pagination")
	}
	for _, obj := range out.Contents {
		key := aws.ToString(obj.Key)
		if !strings.HasPrefix(key, prefix) {
			return v050.Page{}, errors.New("invalid listing key")
		}
		page.Keys = append(page.Keys, key)
	}
	return page, nil
}

// LocalReader is restricted to the disposable in-cluster MinIO fixture. It never
// loads host/environment credentials and cannot be pointed at an AWS endpoint.
// The fixture client is built once so reconciles reuse its connection pool; it
// resolves no credentials, so there is nothing to refresh and no context to
// detach from.
func LocalReader(bucket string) *S3Reader { return &S3Reader{API: localClient(), Bucket: bucket} }

var localClient = sync.OnceValue(func() ObjectAPI {
	cfg := aws.Config{Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "qualification", SecretAccessKey: "qualification-only"}, nil
	}), HTTPClient: &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://minio.celld-test-store.svc:9000")
		o.UsePathStyle = true
		o.RetryMaxAttempts = 2
	})
})
