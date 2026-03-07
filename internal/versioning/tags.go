package versioning

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type VersionTag struct {
	Name      string `json:"name"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	CreatedAt int64  `json:"created_at"`
	CreatedBy string `json:"created_by,omitempty"`
}

// TagStore manages version tags using the metadata store.
type TagStore struct {
	store *metadata.Store
}

func NewTagStore(store *metadata.Store) *TagStore {
	return &TagStore{store: store}
}

func tagKey(bucket, key, tagName string) string {
	return bucket + "\x00" + key + "\x00" + tagName
}

func tagPrefix(bucket, key string) string {
	return bucket + "\x00" + key + "\x00"
}

func (ts *TagStore) PutTag(tag VersionTag) error {
	if tag.CreatedAt == 0 {
		tag.CreatedAt = time.Now().Unix()
	}

	// Verify the version exists
	if _, err := ts.store.GetObjectVersion(tag.Bucket, tag.Key, tag.VersionID); err != nil {
		return fmt.Errorf("version not found: %w", err)
	}

	data, err := json.Marshal(tag)
	if err != nil {
		return err
	}
	return ts.store.PutVersionTag(tagKey(tag.Bucket, tag.Key, tag.Name), data)
}

func (ts *TagStore) GetTags(bucket, key string) ([]VersionTag, error) {
	entries, err := ts.store.ListVersionTags(tagPrefix(bucket, key))
	if err != nil {
		return nil, err
	}

	var tags []VersionTag
	for _, data := range entries {
		var tag VersionTag
		if err := json.Unmarshal(data, &tag); err != nil {
			continue
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

func (ts *TagStore) DeleteTag(bucket, key, tagName string) error {
	return ts.store.DeleteVersionTag(tagKey(bucket, key, tagName))
}

func (ts *TagStore) GetVersionByTag(bucket, key, tagName string) (*VersionTag, error) {
	data, err := ts.store.GetVersionTag(tagKey(bucket, key, tagName))
	if err != nil {
		return nil, err
	}
	var tag VersionTag
	if err := json.Unmarshal(data, &tag); err != nil {
		return nil, err
	}
	return &tag, nil
}
