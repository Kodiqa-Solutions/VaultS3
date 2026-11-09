package metadata

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// NewVersionID returns a version ID for a new version of a key whose current
// version is after (empty for none): the creation time in nanoseconds as 16
// hex digits, then random hex, and always above after.
//
// The highest ID is a key's current version, on every site of active-active
// replication. A local write took the clock's time, so a site whose clock ran
// behind another's gave a newer write a lower ID than a version it had just
// received: the write was current there and not on the other site, for good.
// Taking the time past after keeps the newest write the highest ID wherever
// the clocks stand.
func NewVersionID(after string) string {
	ts := time.Now().UnixNano()
	if prev, ok := VersionIDTime(after); ok && ts <= prev && prev < 1<<63-1 {
		ts = prev + 1
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return fmt.Sprintf("%016x%s", ts, hex.EncodeToString(b))
}

// VersionIDTime is the creation time, in unix nanoseconds, at the start of a
// version ID this server generated.
func VersionIDTime(id string) (int64, bool) {
	if len(id) < 17 {
		return 0, false
	}
	n, err := strconv.ParseUint(id[:16], 16, 64)
	if err != nil || n > 1<<63-1 {
		return 0, false
	}
	return int64(n), true
}
