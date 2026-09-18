package recovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
