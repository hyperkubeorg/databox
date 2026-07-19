// handlers.go routes and serves the core S3 HTTP surface (§14): bucket
// create/list/delete, object PUT/GET/HEAD/DELETE, ListObjectsV2,
// and multipart upload. Every request is authenticated (SigV4) and
// authorized (grants) before any cluster IO happens.
package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/blob"
)

// Key construction: every bucket/object maps under the gateway's root
// prefix (default "/s3/", configurable via Options.RootPrefix, §14):
//
//	bucket marker      /s3/<bucket>                  (plain KV value)
//	object             /s3/<bucket>/<object>         (blob)
//	upload marker      /s3/_multipart/<id>           (plain KV value, JSON)
//	upload part        /s3/_multipart/<id>/<%06d>    (blob)
func (g *gateway) bucketMarkerKey(bucket string) string   { return g.root + bucket }
func (g *gateway) objectKey(bucket, object string) string { return g.root + bucket + "/" + object }
func (g *gateway) bucketPrefix(bucket string) string      { return g.root + bucket + "/" }
func (g *gateway) multipartKey(id string, part int) string {
	return g.root + "_multipart/" + id + "/" + fmt.Sprintf("%06d", part)
}
func (g *gateway) multipartPrefix(id string) string    { return g.root + "_multipart/" + id + "/" }
func (g *gateway) multipartMarkerKey(id string) string { return g.root + "_multipart/" + id }

// router builds the single catch-all handler; S3 routing is by path shape and
// query parameters rather than fixed routes, so one dispatcher is clearest.
func (g *gateway) router() http.Handler {
	return http.HandlerFunc(g.dispatch)
}

// dispatch authenticates the request, then routes by path and method.
func (g *gateway) dispatch(w http.ResponseWriter, r *http.Request) {
	c, err := g.authenticate(r.Context(), r)
	if err != nil {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", err.Error(), r.URL.Path)
		return
	}
	// When the client signed a concrete payload hash, verify the body
	// against it while it streams (bodyhash.go); a mismatch aborts the
	// upload before its manifest would commit.
	wrapPayloadVerification(r)
	// Split the path into bucket and object components.
	p := strings.TrimPrefix(r.URL.Path, "/")
	var bucket, object string
	if i := strings.IndexByte(p, '/'); i >= 0 {
		bucket, object = p[:i], p[i+1:]
	} else {
		bucket = p
	}

	switch {
	case bucket == "":
		if r.Method == http.MethodGet {
			g.listBuckets(w, r, c)
			return
		}
	case object == "":
		g.bucketOp(w, r, c, bucket)
		return
	default:
		g.objectOp(w, r, c, bucket, object)
		return
	}
	writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported operation", r.URL.Path)
}

// --- buckets -----------------------------------------------------------------

// listBuckets returns the buckets the caller may list.
func (g *gateway) listBuckets(w http.ResponseWriter, r *http.Request, c *caller) {
	entries, _, err := g.admin.List(r.Context(), g.root, "", 10000)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), "/")
		return
	}
	var out listAllMyBucketsResult
	out.Owner = ownerXML{ID: c.user, DisplayName: c.user}
	seen := map[string]bool{}
	for _, e := range entries {
		name := strings.TrimPrefix(e.Key, g.root)
		// Bucket markers are single-segment keys under /s3/.
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if !c.authorize(g.bucketPrefix(name), auth.VerbList) {
			continue
		}
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucketXML{
			Name: name, CreationDate: time.Now().UTC().Format(time.RFC3339),
		})
	}
	writeXML(w, http.StatusOK, out)
}

// bucketOp handles operations addressed at a bucket (no object key), keyed by
// method and query: create, delete, and ListObjectsV2.
func (g *gateway) bucketOp(w http.ResponseWriter, r *http.Request, c *caller, bucket string) {
	switch r.Method {
	case http.MethodPut: // CreateBucket
		if !c.authorize(g.bucketPrefix(bucket), auth.VerbWrite) {
			writeS3Error(w, http.StatusForbidden, "AccessDenied", "no write grant on bucket", bucket)
			return
		}
		if _, err := g.admin.Set(r.Context(), g.bucketMarkerKey(bucket), []byte("bucket")); err != nil {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), bucket)
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete: // DeleteBucket (must be empty)
		if !c.authorize(g.bucketPrefix(bucket), auth.VerbDelete) {
			writeS3Error(w, http.StatusForbidden, "AccessDenied", "no delete grant on bucket", bucket)
			return
		}
		objs, _, err := g.admin.List(r.Context(), g.bucketPrefix(bucket), "", 1)
		if err != nil {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), bucket)
			return
		}
		if len(objs) > 0 {
			writeS3Error(w, http.StatusConflict, "BucketNotEmpty", "bucket is not empty", bucket)
			return
		}
		if err := g.admin.Delete(r.Context(), g.bucketMarkerKey(bucket)); err != nil {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), bucket)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet, http.MethodHead: // ListObjectsV2
		if !c.authorize(g.bucketPrefix(bucket), auth.VerbList) {
			writeS3Error(w, http.StatusForbidden, "AccessDenied", "no list grant on bucket", bucket)
			return
		}
		g.listObjects(w, r, bucket)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported bucket operation", bucket)
	}
}

// listObjects implements ListObjectsV2: prefix filtering, delimiter
// grouping into CommonPrefixes (list.go), max-keys paging with an opaque
// continuation token.
func (g *gateway) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxKeys = n
		}
	}
	res, err := collectListing(r.Context(), g.admin, g.bucketPrefix(bucket),
		prefix, delimiter, q.Get("continuation-token"), maxKeys)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), bucket)
		return
	}
	out := listBucketResult{Name: bucket, Prefix: prefix, Delimiter: delimiter, MaxKeys: maxKeys}
	for _, e := range res.objects {
		key := strings.TrimPrefix(e.Key, g.bucketPrefix(bucket))
		size := int64(len(e.Value))
		etag := ""
		if e.Blob {
			if m, err := blob.Decode(e.Value); err == nil {
				size = m.Size
				etag = "\"" + etagFor(m) + "\""
			}
		}
		out.Contents = append(out.Contents, objectXML{
			Key: key, LastModified: time.Now().UTC().Format(time.RFC3339),
			ETag: etag, Size: size, StorageClass: "STANDARD",
		})
	}
	for _, cp := range res.commonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPref{Prefix: cp})
	}
	// KeyCount counts BOTH objects and common prefixes — AWS's accounting.
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	if res.truncated {
		out.IsTruncated = true
		out.NextContinuationToken = res.next
	}
	writeXML(w, http.StatusOK, out)
}

// --- objects -----------------------------------------------------------------

// objectOp handles object-addressed requests, including the multipart
// sub-resources selected by query parameters.
func (g *gateway) objectOp(w http.ResponseWriter, r *http.Request, c *caller, bucket, object string) {
	q := r.URL.Query()
	key := g.objectKey(bucket, object)
	switch r.Method {
	case http.MethodPut:
		if _, ok := q["partNumber"]; ok {
			g.uploadPart(w, r, c, bucket, object)
			return
		}
		g.putObject(w, r, c, key)
	case http.MethodGet:
		if id := q.Get("uploadId"); id != "" { // ListParts
			g.listUploadParts(w, r, c, bucket, object, id)
			return
		}
		g.getObject(w, r, c, key, false)
	case http.MethodHead:
		g.getObject(w, r, c, key, true)
	case http.MethodDelete:
		if id := q.Get("uploadId"); id != "" { // AbortMultipartUpload
			g.abortMultipart(w, r, c, bucket, object, id)
			return
		}
		g.deleteObject(w, r, c, key)
	case http.MethodPost:
		if _, ok := q["uploads"]; ok {
			g.initiateMultipart(w, r, c, bucket, object)
			return
		}
		if id := q.Get("uploadId"); id != "" {
			g.completeMultipart(w, r, c, bucket, object, id)
			return
		}
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "unsupported POST", key)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported object operation", key)
	}
}

// putObject streams the request body into a blob at the object's key.
func (g *gateway) putObject(w http.ResponseWriter, r *http.Request, c *caller, key string) {
	if !c.authorize(key, auth.VerbWrite) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no write grant", key)
		return
	}
	if err := g.admin.PutBlob(r.Context(), key, r.Body, r.Header.Get("Content-Type")); err != nil {
		// A signed-payload-hash mismatch surfaces here as a stream error
		// (bodyhash.go); the blob never committed, so answer BadDigest.
		if bodyMismatch(r) {
			writeS3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch",
				"body does not match the signed x-amz-content-sha256", key)
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	// Report the stored blob's hash as the ETag (via a manifest read).
	if m, err := g.statObject(r.Context(), key); err == nil {
		w.Header().Set("ETag", "\""+etagFor(m)+"\"")
	}
	w.WriteHeader(http.StatusOK)
}

// getObject streams a blob out (or, for HEAD, just its headers).
func (g *gateway) getObject(w http.ResponseWriter, r *http.Request, c *caller, key string, head bool) {
	if !c.authorize(key, auth.VerbRead) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no read grant", key)
		return
	}
	m, err := g.statObject(r.Context(), key)
	if err != nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "object not found", key)
		return
	}
	ct := m.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("ETag", "\""+etagFor(m)+"\"")
	if head {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := g.admin.GetBlob(r.Context(), key, w); err != nil {
		// Headers already sent; nothing more to do but stop.
		return
	}
}

// deleteObject removes an object's blob.
func (g *gateway) deleteObject(w http.ResponseWriter, r *http.Request, c *caller, key string) {
	if !c.authorize(key, auth.VerbDelete) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no delete grant", key)
		return
	}
	if err := g.admin.DeleteBlob(r.Context(), key); err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// statObject reads an object's blob manifest for size/hash/content-type.
func (g *gateway) statObject(ctx context.Context, key string) (*blob.Manifest, error) {
	e, found, err := g.admin.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found || !e.Blob {
		return nil, fmt.Errorf("not found")
	}
	return blob.Decode(e.Value)
}

// --- helpers -----------------------------------------------------------------

// writeXML marshals v as an S3 XML response.
func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

// writeS3Error writes an S3 error document with the given HTTP status.
func writeS3Error(w http.ResponseWriter, status int, code, message, resource string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(s3Error{Code: code, Message: message, Resource: resource})
}
