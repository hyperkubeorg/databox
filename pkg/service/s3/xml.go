// xml.go holds the S3 XML response shapes the gateway emits. Only the fields
// the common SDKs (aws-sdk, boto3, minio-go) read for the core surface are
// included (§14); the wire format is otherwise standard S3.
package s3

import "encoding/xml"

// listAllMyBucketsResult is the ListBuckets response body.
type listAllMyBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	Owner   ownerXML      `xml:"Owner"`
	Buckets bucketListXML `xml:"Buckets"`
}

type bucketListXML struct {
	Bucket []bucketXML `xml:"Bucket"`
}

type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type ownerXML struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// listBucketResult is the ListObjectsV2 response body. KeyCount counts
// Contents plus CommonPrefixes, and MaxKeys caps that same sum — AWS's
// accounting for delimiter listings.
type listBucketResult struct {
	XMLName               xml.Name     `xml:"ListBucketResult"`
	Name                  string       `xml:"Name"`
	Prefix                string       `xml:"Prefix"`
	Delimiter             string       `xml:"Delimiter,omitempty"`
	KeyCount              int          `xml:"KeyCount"`
	MaxKeys               int          `xml:"MaxKeys"`
	IsTruncated           bool         `xml:"IsTruncated"`
	NextContinuationToken string       `xml:"NextContinuationToken,omitempty"`
	Contents              []objectXML  `xml:"Contents"`
	CommonPrefixes        []commonPref `xml:"CommonPrefixes,omitempty"`
}

type objectXML struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPref struct {
	Prefix string `xml:"Prefix"`
}

// initiateMultipartUploadResult is the response to POST ?uploads.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// completeMultipartUpload is the request body of POST ?uploadId=... (the
// client's list of parts). completeMultipartUploadResult is the response.
type completeMultipartUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

// listPartsResult is the response to GET ?uploadId=... (ListParts): the
// parts stored so far for an in-flight multipart upload.
type listPartsResult struct {
	XMLName     xml.Name  `xml:"ListPartsResult"`
	Bucket      string    `xml:"Bucket"`
	Key         string    `xml:"Key"`
	UploadID    string    `xml:"UploadId"`
	IsTruncated bool      `xml:"IsTruncated"`
	Parts       []partXML `xml:"Part"`
}

// partXML is one stored part in a ListPartsResult.
type partXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
	Size       int64  `xml:"Size"`
}

// s3Error is the standard S3 error document.
type s3Error struct {
	XMLName  xml.Name `xml:"Error"`
	Code     string   `xml:"Code"`
	Message  string   `xml:"Message"`
	Resource string   `xml:"Resource,omitempty"`
}
