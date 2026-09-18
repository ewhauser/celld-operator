// Package recovery provides narrowly scoped read-only primary S3 evidence.
package recovery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region),
		config.WithSharedConfigFiles([]string{}), config.WithSharedCredentialsFiles([]string{}),
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled), config.WithRetryMaxAttempts(2),
		config.WithHTTPClient(&http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	if err != nil {
		return nil, err
	}
	api := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = nil
		o.EndpointResolverV2 = s3.NewDefaultEndpointResolverV2()
		o.UseAccelerate = false
		o.EndpointOptions.UseDualStackEndpoint = aws.DualStackEndpointStateDisabled
		o.UseARNRegion = false
	})
	return &S3Reader{API: api, Bucket: bucket}, nil
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
