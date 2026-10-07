package metadata

import "encoding/json"

// BucketHasData reports whether a bucket still holds anything that deleting it
// would destroy: a current object, a noncurrent version, or a delete marker.
//
// Bucket deletion used to decide this from a walk of the bucket's directory on
// disk. That walk skips .vs/, which is where every object in a versioning
// enabled bucket keeps its bytes, so such a bucket always looked empty and was
// removed with everything in it, objects under retention or legal hold
// included. The metadata store is the authority for what a bucket holds, and on
// a cluster it is the same on every node, where a directory walk sees only this
// node's share. An error is returned as is: a caller must refuse the delete
// rather than treat an unreadable store as an empty bucket.
func BucketHasData(s StoreAPI, bucket string) (bool, error) {
	latest, _, err := s.ListLatestObjects(bucket, "", "", 1)
	if err != nil {
		return false, err
	}
	if len(latest) > 0 {
		return true, nil
	}
	versions, _, err := s.ListObjectVersions(bucket, "", "", "", 1)
	if err != nil {
		return false, err
	}
	return len(versions) > 0, nil
}

// UnmarshalJSON reads an object record and repairs one known defect in it.
// Snowball imports and POST uploads before 5.0.0 stored LastModified in
// nanoseconds where every reader expects seconds, so those objects showed a
// date some 50 billion years ahead, which boto3 refuses to parse, and listing a
// bucket holding one failed outright. A seconds value stays below 1e12 until
// the year 33658, so anything larger is a nanosecond stamp.
func (m *ObjectMeta) UnmarshalJSON(data []byte) error {
	type plain ObjectMeta
	if err := json.Unmarshal(data, (*plain)(m)); err != nil {
		return err
	}
	if m.LastModified > 1e12 {
		m.LastModified /= 1e9
	}
	return nil
}
