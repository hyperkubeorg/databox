// s3dest.go implements Dest against S3-compatible object storage using
// the minimal SigV4 signer in sigv4.go — plain net/http, path-style URLs
// (http(s)://endpoint/bucket/key), custom endpoints supported so MinIO,
// Ceph RGW, and the databox S3 gateway itself all work as destinations.
package backup

import (
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// s3Dest is one bucket+prefix on one S3 endpoint.
type s3Dest struct {
	endpoint string // scheme://host[:port], no trailing slash
	bucket   string
	prefix   string // key prefix inside the bucket ("" = bucket root)
	keyID    string
	secret   string
	region   string
	client   *http.Client
}

// newS3Dest validates the credentials/endpoint combination and returns
// the destination. No network call is made here — the first Put/List
// surfaces connectivity problems, which keeps job-record error reporting
// in one place.
func newS3Dest(bucket, prefix string, creds Credentials) (*s3Dest, error) {
	endpoint := creds.S3Endpoint
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid s3 endpoint %q (want http(s)://host[:port])", endpoint)
	}
	region := creds.Region
	if region == "" {
		region = "us-east-1"
	}
	return &s3Dest{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		bucket:   bucket,
		prefix:   prefix,
		keyID:    creds.AccessKey,
		secret:   creds.SecretKey,
		region:   region,
		client:   &http.Client{Timeout: 10 * time.Minute}, // large blob files
	}, nil
}

// key maps a Dest-relative path to the full object key inside the bucket.
func (d *s3Dest) key(path string) string {
	if d.prefix == "" {
		return path
	}
	return d.prefix + "/" + path
}

// objectURL builds the path-style URL for one object.
func (d *s3Dest) objectURL(path string) string {
	return d.endpoint + "/" + d.bucket + awsURIEncodePath("/"+d.key(path))
}

// do signs and executes one request, mapping non-2xx to an error carrying
// the S3 error body (truncated) for diagnosability.
func (d *s3Dest) do(req *http.Request, payloadHash string) (*http.Response, error) {
	SignRequestV4(req, d.keyID, d.secret, d.region, "s3", payloadHash, time.Now())
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("s3 %s %s: %w: %s", req.Method, req.URL.Path, fs.ErrNotExist, strings.TrimSpace(string(body)))
	}
	return nil, fmt.Errorf("s3 %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
}

// Put uploads a file. The reader is spooled to a temporary file first so
// the request can carry a Content-Length (S3 rejects plain chunked
// uploads) and is signed with UNSIGNED-PAYLOAD so multi-gigabyte blob
// files are not hashed twice.
func (d *s3Dest) Put(path string, r io.Reader) error {
	tmp, err := os.CreateTemp("", "databox-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, r)
	if err != nil {
		return fmt.Errorf("spool %s: %w", path, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, d.objectURL(path), tmp)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := d.do(req, UnsignedPayload)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// Get streams a file down. The caller closes the returned body.
func (d *s3Dest) Get(path string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, d.objectURL(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.do(req, EmptyPayloadHash)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// listBucketResult is the slice of ListObjectsV2 XML we care about.
type listBucketResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// List pages through ListObjectsV2, returning Dest-relative paths.
func (d *s3Dest) List(prefix string) ([]string, error) {
	var out []string
	token := ""
	base := d.key(prefix)
	strip := d.prefix
	if strip != "" {
		strip += "/"
	}
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {base}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := http.NewRequest(http.MethodGet, d.endpoint+"/"+d.bucket+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := d.do(req, EmptyPayloadHash)
		if err != nil {
			return nil, err
		}
		var res listBucketResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("parse s3 list response: %w", err)
		}
		for _, c := range res.Contents {
			out = append(out, strings.TrimPrefix(c.Key, strip))
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}
