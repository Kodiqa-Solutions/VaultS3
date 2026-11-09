package metadata

import (
	"fmt"
	"math/rand"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func r7check(t *testing.T, s *Store, step string) {
	t.Helper()
	got, err := s.BucketStoredUsage("b")
	if err != nil {
		t.Fatal(err)
	}
	if m := measured(t, s, "b"); m != got {
		t.Fatalf("%s: kept figure %+v, records add up to %+v", step, got, m)
	}
}

// Random sequences of every tracked store write: the kept figure must equal
// the measure after each.
func TestStoredFigureMatchesTheRecordsAfterRandomWrites(t *testing.T) {
	for _, ordering := range []bool{false, true} {
		old := versionOrdering.Load()
		versionOrdering.Store(ordering)
		for seed := int64(0); seed < 30; seed++ {
			s := usageStore(t)
			rng := rand.New(rand.NewSource(seed))
			keys := []string{"k1", "k2", "k3"}
			vids := []string{"", "null", "0000000000000001aaaaaaaa", "0000000000000002bbbbbbbb", "0000000000000003cccccccc"}
			for i := 0; i < 60; i++ {
				k := keys[rng.Intn(len(keys))]
				v := vids[rng.Intn(len(vids))]
				m := ObjectMeta{Bucket: "b", Key: k, VersionID: v, Size: int64(rng.Intn(100)), IsLatest: rng.Intn(2) == 0, DeleteMarker: rng.Intn(5) == 0}
				var op string
				switch rng.Intn(9) {
				case 0:
					op = "PutObjectMeta"
					s.PutObjectMeta(m)
				case 1:
					op = "PutObjectVersion"
					s.PutObjectVersion(m)
				case 2:
					op = "DeleteObjectVersion"
					s.DeleteObjectVersion("b", k, v)
				case 3:
					op = "SetLatestVersion"
					s.SetLatestVersion("b", k, v)
				case 4:
					op = "UpdateObjectVersionMeta"
					s.UpdateObjectVersionMeta(m)
				case 5:
					op = "DeleteObjectMeta"
					s.DeleteObjectMeta("b", k)
				case 6:
					op = "Batch"
					s.PutObjectMetaBatch([]ObjectMeta{m, {Bucket: "b", Key: keys[rng.Intn(3)], Size: 9}})
				case 7:
					op = "forget figure"
					s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketStoredBucket).Delete([]byte("b")) })
				case 8:
					if rng.Intn(10) == 0 {
						op = "DeleteBucketObjectMeta"
						s.DeleteBucketObjectMeta("b")
					}
				}
				r7check(t, s, fmt.Sprintf("ordering=%v seed=%d step=%d op=%s meta=%+v", ordering, seed, i, op, m))
			}
		}
		versionOrdering.Store(old)
	}
}
