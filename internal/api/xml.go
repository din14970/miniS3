package api

import "encoding/xml"

const xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

type ErrorResult struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   Owner    `xml:"Owner"`
	Buckets struct {
		Bucket []BucketEntry `xml:"Bucket"`
	} `xml:"Buckets"`
}

type BucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type Contents struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

type ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix,omitempty"`
	Marker                string         `xml:"Marker,omitempty"`
	NextMarker            string         `xml:"NextMarker,omitempty"`
	KeyCount              int            `xml:"KeyCount,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes,omitempty"`
	Contents              []Contents     `xml:"Contents,omitempty"`
}

type VersionEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

type DeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

type ListVersionsResult struct {
	XMLName            xml.Name            `xml:"ListVersionsResult"`
	Xmlns              string              `xml:"xmlns,attr"`
	Name               string              `xml:"Name"`
	Prefix             string              `xml:"Prefix,omitempty"`
	KeyMarker          string              `xml:"KeyMarker,omitempty"`
	VersionIDMarker    string              `xml:"VersionIdMarker,omitempty"`
	NextKeyMarker      string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string             `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys            int                 `xml:"MaxKeys"`
	Delimiter          string              `xml:"Delimiter,omitempty"`
	IsTruncated        bool                `xml:"IsTruncated"`
	EncodingType       string              `xml:"EncodingType,omitempty"`
	Versions           []VersionEntry      `xml:"Version,omitempty"`
	DeleteMarkers      []DeleteMarkerEntry `xml:"DeleteMarker,omitempty"`
	CommonPrefixes     []CommonPrefix      `xml:"CommonPrefixes,omitempty"`
}

type VersioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
	Status  string   `xml:"Status,omitempty"`
}

type LocationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

type CreateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CreateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type CompleteMultipartUploadRequest struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

type PartEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type ListPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	Xmlns                string      `xml:"xmlns,attr"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadID             string      `xml:"UploadId"`
	Initiated            string      `xml:"Initiated"`
	StorageClass         string      `xml:"StorageClass"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Parts                []PartEntry `xml:"Part,omitempty"`
}

type UploadEntry struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiated    string `xml:"Initiated"`
	StorageClass string `xml:"StorageClass"`
}

type ListMultipartUploadsResult struct {
	XMLName             xml.Name       `xml:"ListMultipartUploadsResult"`
	Xmlns               string         `xml:"xmlns,attr"`
	Bucket              string         `xml:"Bucket"`
	Prefix              string         `xml:"Prefix,omitempty"`
	Delimiter           string         `xml:"Delimiter,omitempty"`
	KeyMarker           string         `xml:"KeyMarker,omitempty"`
	UploadIDMarker      string         `xml:"UploadIdMarker,omitempty"`
	NextKeyMarker       string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker  string         `xml:"NextUploadIdMarker,omitempty"`
	MaxUploads          int            `xml:"MaxUploads"`
	IsTruncated         bool           `xml:"IsTruncated"`
	EncodingType        string         `xml:"EncodingType,omitempty"`
	Uploads             []UploadEntry  `xml:"Upload,omitempty"`
	CommonPrefixes      []CommonPrefix `xml:"CommonPrefixes,omitempty"`
}

type DeleteRequestObject struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

type DeleteRequest struct {
	XMLName xml.Name              `xml:"Delete"`
	Quiet   bool                  `xml:"Quiet"`
	Objects []DeleteRequestObject `xml:"Object"`
}

type DeletedEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId,omitempty"`
	DeleteMarker bool   `xml:"DeleteMarker,omitempty"`
}

type DeleteErrorEntry struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

type DeleteResult struct {
	XMLName xml.Name           `xml:"DeleteResult"`
	Xmlns   string             `xml:"xmlns,attr"`
	Deleted []DeletedEntry     `xml:"Deleted,omitempty"`
	Errors  []DeleteErrorEntry `xml:"Error,omitempty"`
}

type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr"`
	TagSet  struct {
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"Tag"`
	} `xml:"TagSet"`
}

type ACL struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   Owner    `xml:"Owner"`
	Grants  struct {
		Grant []struct {
			Grantee struct {
				Type        string `xml:"xsi:type,attr"`
				ID          string `xml:"ID"`
				DisplayName string `xml:"DisplayName"`
			} `xml:"Grantee"`
			Permission string `xml:"Permission"`
		} `xml:"Grant"`
	} `xml:"AccessControlList"`
}
