package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// PostUpload handles POST /{bucket} for HTML form-based uploads with policy document validation.
func (h *ObjectHandler) PostUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	if err := r.ParseMultipartForm(128 * 1024 * 1024); err != nil {
		writeS3Error(w, "MalformedPOSTRequest", "Could not parse multipart form", http.StatusBadRequest)
		return
	}

	key := r.FormValue("key")
	if key == "" {
		writeS3Error(w, "InvalidArgument", "Missing required field: key", http.StatusBadRequest)
		return
	}

	// Replace ${filename} in key template
	file, header, err := r.FormFile("file")
	if err != nil {
		writeS3Error(w, "InvalidArgument", "Missing required field: file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if strings.Contains(key, "${filename}") {
		key = strings.ReplaceAll(key, "${filename}", header.Filename)
	}

	// Validate policy if present
	policyB64 := r.FormValue("Policy")
	if policyB64 != "" {
		signature := r.FormValue("X-Amz-Signature")
		credential := r.FormValue("X-Amz-Credential")
		if signature == "" || credential == "" {
			writeS3Error(w, "AccessDenied", "Missing signature fields", http.StatusForbidden)
			return
		}

		if err := h.validatePostPolicy(policyB64, signature, credential, r); err != nil {
			writeS3Error(w, "AccessDenied", err.Error(), http.StatusForbidden)
			return
		}
	}

	// Check quota
	if !h.checkQuota(w, bucket, header.Size) {
		return
	}

	// Store the object
	size, etag, err := h.engine.PutObject(bucket, key, file, header.Size)
	if err != nil {
		writeS3Error(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}

	ct := header.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	h.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:       bucket,
		Key:          key,
		Size:         size,
		ETag:         etag,
		ContentType:  ct,
		LastModified: time.Now().UTC().UnixNano(),
	})

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("Location", fmt.Sprintf("/%s/%s", bucket, key))
	w.WriteHeader(http.StatusNoContent)
}

func (h *ObjectHandler) validatePostPolicy(policyB64, signature, credential string, r *http.Request) error {
	policyJSON, err := base64.StdEncoding.DecodeString(policyB64)
	if err != nil {
		return fmt.Errorf("invalid policy encoding")
	}

	var policy postPolicy
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		return fmt.Errorf("invalid policy document")
	}

	// Check expiration
	expiry, err := time.Parse(time.RFC3339, policy.Expiration)
	if err != nil {
		expiry, err = time.Parse("2006-01-02T15:04:05.000Z", policy.Expiration)
		if err != nil {
			return fmt.Errorf("invalid expiration format")
		}
	}
	if time.Now().UTC().After(expiry) {
		return fmt.Errorf("policy has expired")
	}

	// Validate conditions
	for _, cond := range policy.Conditions {
		if err := validatePostCondition(cond, r); err != nil {
			return err
		}
	}

	// Verify signature: HMAC-SHA256 of policyB64
	parts := strings.Split(credential, "/")
	if len(parts) < 5 {
		return fmt.Errorf("invalid credential format")
	}
	accessKey := parts[0]
	dateStamp := parts[1]
	region := parts[2]
	service := parts[3]

	// Look up secret key
	key, err := h.store.GetAccessKey(accessKey)
	if err != nil {
		return fmt.Errorf("unknown access key")
	}

	signingKey := deriveSigningKey(key.SecretKey, dateStamp, region, service)
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte(policyB64))
	expected := fmt.Sprintf("%x", mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return fmt.Errorf("signature does not match")
	}

	return nil
}

type postPolicy struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

func validatePostCondition(raw json.RawMessage, r *http.Request) error {
	// Conditions can be:
	// {"bucket": "mybucket"} — exact match
	// ["starts-with", "$key", "uploads/"] — starts-with
	// ["content-length-range", 0, 10485760] — content-length-range
	var exact map[string]string
	if err := json.Unmarshal(raw, &exact); err == nil {
		for field, expected := range exact {
			actual := r.FormValue(field)
			if actual == "" && field == "bucket" {
				// bucket comes from URL, not form
				continue
			}
			if actual != expected {
				return fmt.Errorf("condition not met: %s expected %q got %q", field, expected, actual)
			}
		}
		return nil
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil // skip unknown
	}

	if len(arr) == 3 {
		var op string
		json.Unmarshal(arr[0], &op)

		if strings.EqualFold(op, "starts-with") {
			var field, prefix string
			json.Unmarshal(arr[1], &field)
			json.Unmarshal(arr[2], &prefix)
			field = strings.TrimPrefix(field, "$")
			actual := r.FormValue(field)
			if prefix != "" && !strings.HasPrefix(actual, prefix) {
				return fmt.Errorf("starts-with condition not met for %s", field)
			}
		} else if strings.EqualFold(op, "content-length-range") {
			var min, max int64
			json.Unmarshal(arr[1], &min)
			json.Unmarshal(arr[2], &max)
			_, fh, err := r.FormFile("file")
			if err == nil {
				size := fh.Size
				if size < min || size > max {
					return fmt.Errorf("file size %d not in range [%d, %d]", size, min, max)
				}
			}
		}
	}

	return nil
}
