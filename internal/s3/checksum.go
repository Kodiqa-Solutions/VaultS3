package s3

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// checksumFromRequest computes and validates checksums from request headers.
// It reads the body fully to compute checksums, returning the body bytes.
func checksumFromRequest(r *http.Request, body []byte) (sha256sum, crc32sum, crc32csum, sha1sum string, err error) {
	if v := r.Header.Get("X-Amz-Checksum-Sha256"); v != "" {
		h := sha256.Sum256(body)
		computed := base64.StdEncoding.EncodeToString(h[:])
		if v != computed {
			return "", "", "", "", errChecksumMismatch("SHA256")
		}
		sha256sum = computed
	} else if r.Header.Get("X-Amz-Trailer") == "x-amz-checksum-sha256" {
		h := sha256.Sum256(body)
		sha256sum = base64.StdEncoding.EncodeToString(h[:])
	}

	if v := r.Header.Get("X-Amz-Checksum-Crc32"); v != "" {
		h := crc32.ChecksumIEEE(body)
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, h)
		computed := base64.StdEncoding.EncodeToString(b)
		if v != computed {
			return "", "", "", "", errChecksumMismatch("CRC32")
		}
		crc32sum = computed
	}

	if v := r.Header.Get("X-Amz-Checksum-Crc32c"); v != "" {
		table := crc32.MakeTable(crc32.Castagnoli)
		h := crc32.Checksum(body, table)
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, h)
		computed := base64.StdEncoding.EncodeToString(b)
		if v != computed {
			return "", "", "", "", errChecksumMismatch("CRC32C")
		}
		crc32csum = computed
	}

	if v := r.Header.Get("X-Amz-Checksum-Sha1"); v != "" {
		h := sha1.Sum(body)
		computed := base64.StdEncoding.EncodeToString(h[:])
		if v != computed {
			return "", "", "", "", errChecksumMismatch("SHA1")
		}
		sha1sum = computed
	}

	return sha256sum, crc32sum, crc32csum, sha1sum, nil
}

type checksumError struct {
	algorithm string
}

func (e *checksumError) Error() string {
	return "Checksum " + e.algorithm + " does not match"
}

func errChecksumMismatch(algo string) error {
	return &checksumError{algorithm: algo}
}

// setChecksumHeaders emits checksum headers on GET/HEAD responses.
func setChecksumHeaders(w http.ResponseWriter, meta *metadata.ObjectMeta) {
	if meta.ChecksumSHA256 != "" {
		w.Header().Set("X-Amz-Checksum-Sha256", meta.ChecksumSHA256)
	}
	if meta.ChecksumCRC32 != "" {
		w.Header().Set("X-Amz-Checksum-Crc32", meta.ChecksumCRC32)
	}
	if meta.ChecksumCRC32C != "" {
		w.Header().Set("X-Amz-Checksum-Crc32c", meta.ChecksumCRC32C)
	}
	if meta.ChecksumSHA1 != "" {
		w.Header().Set("X-Amz-Checksum-Sha1", meta.ChecksumSHA1)
	}
}

// computeChecksums computes all supported checksums from a reader.
func computeChecksums(r io.Reader) (sha256sum, crc32sum, crc32csum, sha1sum string, body []byte, err error) {
	body, err = io.ReadAll(r)
	if err != nil {
		return
	}

	h256 := sha256.Sum256(body)
	sha256sum = base64.StdEncoding.EncodeToString(h256[:])

	c32 := crc32.ChecksumIEEE(body)
	b32 := make([]byte, 4)
	binary.BigEndian.PutUint32(b32, c32)
	crc32sum = base64.StdEncoding.EncodeToString(b32)

	table := crc32.MakeTable(crc32.Castagnoli)
	c32c := crc32.Checksum(body, table)
	b32c := make([]byte, 4)
	binary.BigEndian.PutUint32(b32c, c32c)
	crc32csum = base64.StdEncoding.EncodeToString(b32c)

	h1 := sha1.Sum(body)
	sha1sum = base64.StdEncoding.EncodeToString(h1[:])

	return
}
