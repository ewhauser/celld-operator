package recovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

func TestS3ReadOnlyWireAndFaults(t *testing.T) {
	for _, scenario := range []string{"success", "denied", "missing", "partial", "oversize", "bad-page", "page-failure", "cancel", "loss"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
					t.Error("unsigned or mutating request")
				}
				if scenario == "cancel" {
					<-r.Context().Done()
					return
				}
				if scenario == "denied" {
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				if scenario == "missing" {
					http.Error(w, "missing", 404)
					return
				}
				if r.URL.Query().Get("list-type") == "2" {
					prefix := r.URL.Query().Get("prefix")
					token := r.URL.Query().Get("continuation-token")
					if scenario == "bad-page" {
						_, _ = fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)
						return
					}
					if prefix == "nodes/" && token == "" {
						_, _ = fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>second</NextContinuationToken><Contents><Key>nodes/a.json</Key></Contents></ListBucketResult>`)
						return
					}
					if scenario == "page-failure" && token == "second" {
						http.Error(w, "down", http.StatusServiceUnavailable)
						return
					}
					if scenario == "loss" && prefix == "log/" {
						_, _ = fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>log/old/g.e1.loss.json</Key></Contents></ListBucketResult>`)
						return
					}
					_, _ = fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
					return
				}
				if r.URL.Path != "/bucket/nodes/a.json" {
					t.Errorf("unexpected body read %s", r.URL.Path)
				}
				if scenario == "partial" {
					w.Header().Set("Content-Length", "999")
					_, _ = fmt.Fprint(w, "{}")
					return
				}
				if scenario == "oversize" {
					_, _ = fmt.Fprint(w, strings.Repeat(" ", (1<<20)+1))
					return
				}
				_, _ = fmt.Fprintf(w, `{"node":"a","ownership_index_generation":"g","peer_protocol":5,"expires_ms":%d}`, time.Now().Add(time.Minute).UnixMilli())
			}))
			defer server.Close()
			api := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1})
			reader := &S3Reader{API: api, Bucket: "bucket"}
			adapter, _ := v050.New(v050.Image)
			ctx := t.Context()
			if scenario == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			inv, err := adapter.Inventory(ctx, reader, time.Now)
			if scenario == "success" {
				if err != nil || len(inv.Nodes) != 1 || calls.Load() != 4 {
					t.Fatalf("inventory: %+v %v calls=%d", inv, err, calls.Load())
				}
			} else if err == nil {
				t.Fatal("fault accepted")
			}
			if scenario == "loss" && inv.Loss == "" {
				t.Fatal("loss lost")
			}
			before := calls.Load()
			if _, err := reader.Get(t.Context(), "log/a/bundle"); err == nil {
				t.Fatal("bundle read allowed")
			}
			if _, err := reader.List(t.Context(), "application/", ""); err == nil {
				t.Fatal("application list allowed")
			}
			if calls.Load() != before {
				t.Fatal("out of scope network request")
			}
		})
	}
}

// Configuration inspection uses dummy environment credentials and never sends
// a request. Endpoint environment overrides must not redirect primary evidence.
func TestAWSConfigurationIsExplicitAndReadOnly(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_ENDPOINT_URL", "http://not-the-primary.invalid")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://not-s3.invalid")
	r, err := AWSReader(t.Context(), "reserved-bucket", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	opts := r.API.(*s3.Client).Options()
	if opts.Region != "us-west-2" || opts.BaseEndpoint != nil || opts.RetryMaxAttempts != 2 {
		t.Fatalf("unsafe client options: region=%s endpoint=%v", opts.Region, opts.BaseEndpoint)
	}
}

func TestCredentialResolutionCancellation(t *testing.T) {
	api := s3.New(s3.Options{Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) { <-ctx.Done(); return aws.Credentials{}, ctx.Err() })})
	r := S3Reader{API: api, Bucket: "reserved-bucket"}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Get(ctx, "nodes/a.json"); err == nil {
		t.Fatal("credential timeout accepted")
	}
}

// The reconcile path calls the reader factory every pass; each construction
// would otherwise re-resolve credentials over STS and open new connections.
func TestClientCacheConstructsOncePerRegion(t *testing.T) {
	var constructions atomic.Int64
	cache := &ClientCache{newClient: func(ctx context.Context, region string) (*s3.Client, error) {
		constructions.Add(1)
		return awsClient(ctx, region)
	}}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	first, err := cache.Reader("reserved-bucket", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := range 32 {
		group.Go(func() {
			if _, err := cache.Reader(fmt.Sprintf("reserved-bucket-%d", i%2), "us-west-2"); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if got := constructions.Load(); got != 1 {
		t.Fatalf("constructed %d clients for one region", got)
	}
	// Distinct buckets share the region's client but never each other's bucket.
	other, err := cache.Reader("other-bucket", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if other.API != first.API || other.Bucket == first.Bucket {
		t.Fatalf("bucket %q did not share the region client, or shared a bucket", other.Bucket)
	}
	// A second region is a separate client and keeps the same hardening.
	east, err := cache.Reader("reserved-bucket", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if constructions.Load() != 2 || east.API == first.API {
		t.Fatal("second region reused the first region's client")
	}
	opts := east.API.(*s3.Client).Options()
	if opts.Region != "us-east-1" || opts.BaseEndpoint != nil || opts.RetryMaxAttempts != 2 {
		t.Fatalf("cached client lost hardening: region=%s endpoint=%v retries=%d", opts.Region, opts.BaseEndpoint, opts.RetryMaxAttempts)
	}
}

// A construction failure must not be remembered: the next reconcile retries.
func TestClientCacheDoesNotCacheConstructionFailure(t *testing.T) {
	var constructions atomic.Int64
	failure := errors.New("credential resolution failed")
	cache := &ClientCache{newClient: func(ctx context.Context, region string) (*s3.Client, error) {
		if constructions.Add(1) <= 2 {
			return nil, failure
		}
		return awsClient(ctx, region)
	}}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	for range 2 {
		if _, err := cache.Reader("reserved-bucket", "us-west-2"); !errors.Is(err, failure) {
			t.Fatalf("construction failure not surfaced: %v", err)
		}
	}
	if _, err := cache.Reader("reserved-bucket", "us-west-2"); err != nil {
		t.Fatalf("retry after failure did not construct: %v", err)
	}
	if got := constructions.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	if _, err := cache.Reader("reserved-bucket", "us-west-2"); err != nil || constructions.Load() != 3 {
		t.Fatalf("successful client not cached: err=%v attempts=%d", err, constructions.Load())
	}
}

// LocalReader must not rebuild the fixture client (and its connection pool) per
// reconcile either, while still scoping each reader to its own bucket.
func TestLocalReaderSharesOneClient(t *testing.T) {
	first, second := LocalReader("one"), LocalReader("two")
	if first.API != second.API {
		t.Fatal("fixture client rebuilt per call")
	}
	if first.Bucket != "one" || second.Bucket != "two" {
		t.Fatalf("bucket scope lost: %q %q", first.Bucket, second.Bucket)
	}
	opts := first.API.(*s3.Client).Options()
	if opts.BaseEndpoint == nil || *opts.BaseEndpoint != "http://minio.celld-test-store.svc:9000" || !opts.UsePathStyle || opts.RetryMaxAttempts != 2 {
		t.Fatalf("fixture client options changed: %v", opts.BaseEndpoint)
	}
}
