package erasure

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"hash/crc32"
	"strconv"
	"time"
)

// ShardMeta holds metadata for an erasure-coded object.
// Stored alongside the shards to enable reconstruction.
//
// Two layouts exist on disk. Objects written before generations were added
// keep their shards directly under .ec/{key}/ and have no Generation. Newer
// objects put each write's shards under .ec/{key}/{generation}/, and meta.json
// (always at .ec/{key}/meta.json) names the generation that is current. A new
// write therefore never touches the shards a reader or the previous version is
// using, and switching meta.json, which the inner engine replaces atomically,
// is what makes the new version visible.
type ShardMeta struct {
	OriginalSize int64     `json:"original_size"`
	DataShards   int       `json:"data_shards"`
	ParityShards int       `json:"parity_shards"`
	BlockSize    int64     `json:"block_size"`
	ShardSizes   []int64   `json:"shard_sizes"` // actual size of each shard file
	ETag         string    `json:"etag"`
	CreatedAt    time.Time `json:"created_at"`

	// Generation is the subdirectory holding this version's shards. Empty means
	// the original layout, shards directly under .ec/{key}/.
	Generation string `json:"generation,omitempty"`

	// ShardCRC holds, per shard, the CRC-32C of every CRCStripe bytes of that
	// shard (the last stripe may be shorter), big-endian and base64 encoded. A
	// shard whose bytes disagree is treated as missing and rebuilt from parity,
	// so a corrupt-but-present shard is never served. Objects written without
	// checksums leave both fields empty and read unchecked, as before.
	ShardCRC  []string `json:"shard_crc32c,omitempty"`
	CRCStripe int64    `json:"crc_stripe,omitempty"`
}

func (m *ShardMeta) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

func UnmarshalShardMeta(data []byte) (*ShardMeta, error) {
	var m ShardMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// shardPath returns where shard i of this version is stored, in whichever
// layout the version was written.
func (m *ShardMeta) shardPath(key string, i int) string {
	if m.Generation == "" {
		return shardKey(key, i)
	}
	return ecPrefix(key) + m.Generation + "/" + shardName(i)
}

func (m *ShardMeta) totalShards() int { return m.DataShards + m.ParityShards }

// crcTable is the Castagnoli polynomial, hardware accelerated on amd64 and arm64.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// crcStripeBytes is the checksum granularity for new writes. It matches the
// degraded reader's stripe, so a stripe that fails its checksum is recovered
// with exactly the parity reads that stripe needs.
const crcStripeBytes = degradedStripeBytes

// hasChecksums reports whether this version carries per-stripe checksums.
// A stripe size beyond what any writer uses is ignored rather than trusted,
// since the reader allocates one stripe per shard.
func (m *ShardMeta) hasChecksums() bool {
	return m.CRCStripe > 0 && m.CRCStripe <= 64<<20 && len(m.ShardCRC) == m.totalShards()
}

// stripeOK reports whether data, which starts at in-shard offset off, matches
// the stored checksum of shard i. off must be a multiple of CRCStripe and data
// must run to the end of that stripe (or of the shard). A version without
// checksums, or a call that does not line up with a stripe, is reported as OK
// because there is nothing to compare against.
func (m *ShardMeta) stripeOK(i int, off int64, data []byte) bool {
	if !m.hasChecksums() || off%m.CRCStripe != 0 {
		return true
	}
	sums, err := base64.StdEncoding.DecodeString(m.ShardCRC[i])
	if err != nil {
		return false
	}
	for pos := int64(0); pos < int64(len(data)); pos += m.CRCStripe {
		idx := (off + pos) / m.CRCStripe
		if (idx+1)*4 > int64(len(sums)) {
			return false
		}
		end := pos + m.CRCStripe
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		if crc32.Checksum(data[pos:end], crcTable) != binary.BigEndian.Uint32(sums[idx*4:]) {
			return false
		}
	}
	return true
}

// stripeSummer computes the per-stripe checksums of a shard as it is written.
type stripeSummer struct {
	stripe int64
	cur    hash.Hash32
	n      int64
	sums   []byte
}

func newStripeSummer(stripe int64) *stripeSummer {
	return &stripeSummer{stripe: stripe, cur: crc32.New(crcTable)}
}

func (s *stripeSummer) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		take := s.stripe - s.n
		if take > int64(len(p)) {
			take = int64(len(p))
		}
		s.cur.Write(p[:take])
		s.n += take
		p = p[take:]
		if s.n == s.stripe {
			s.sums = binary.BigEndian.AppendUint32(s.sums, s.cur.Sum32())
			s.cur.Reset()
			s.n = 0
		}
	}
	return total, nil
}

// encoded closes the last partial stripe and returns the stored form.
func (s *stripeSummer) encoded() string {
	if s.n > 0 {
		s.sums = binary.BigEndian.AppendUint32(s.sums, s.cur.Sum32())
		s.cur.Reset()
		s.n = 0
	}
	return base64.StdEncoding.EncodeToString(s.sums)
}

// newGeneration names the directory for one write's shards. It is unique per
// write, so two writes of the same key never share a shard file.
func newGeneration() string {
	var r [6]byte
	rand.Read(r[:])
	return "gen-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + hex.EncodeToString(r[:])
}

// shardKey returns the original-layout storage key for a shard index:
// {bucket}/.ec/{key}/shard-{index}. Versions with a Generation use shardPath.
func shardKey(key string, index int) string {
	return ecPrefix(key) + shardName(index)
}

// metaKey returns the storage key for the shard metadata.
func metaKey(key string) string {
	return ecPrefix(key) + "meta.json"
}

// ecPrefix returns the erasure coding prefix for an object key.
func ecPrefix(key string) string {
	return ".ec/" + key + "/"
}

func shardName(index int) string {
	const digits = "0123456789"
	if index < 10 {
		return "shard-0" + string(digits[index])
	}
	return "shard-" + string(digits[index/10]) + string(digits[index%10])
}
