package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func evalPolicy(t *testing.T, store *metadata.Store, name, action string) bool {
	t.Helper()
	p, err := store.GetIAMPolicy(name)
	if err != nil {
		t.Fatal(err)
	}
	var pol iam.Policy
	if err := json.Unmarshal([]byte(p.Document), &pol); err != nil {
		t.Fatal(err)
	}
	return iam.Evaluate([]iam.Policy{pol}, action, "arn:aws:s3:::b/k")
}

// Requests now carry their own AWS action names, so the old built-in documents
// left ReadWriteAccess unable to tag or upload in parts. An installation still
// holding the old documents gets the new ones, and one an operator edited keeps
// its edit.
func TestBuiltinPoliciesCoverTheirNamesAndUpgrade(t *testing.T) {
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.CreateIAMPolicy(metadata.IAMPolicy{Name: "ReadOnlyAccess", Document: oldReadOnlyAccess})
	store.CreateIAMPolicy(metadata.IAMPolicy{Name: "ReadWriteAccess", Document: oldReadWriteAccess})
	edited := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["*"]}]}`
	store.CreateIAMPolicy(metadata.IAMPolicy{Name: "FullAccess", Document: edited})

	initBuiltinPolicies(store)

	for _, a := range []string{"s3:PutObject", "s3:PutObjectTagging", "s3:AbortMultipartUpload", "s3:GetObjectVersion", "s3:ListBucketVersions", "s3:DeleteObject"} {
		if !evalPolicy(t, store, "ReadWriteAccess", a) {
			t.Errorf("ReadWriteAccess does not allow %s after the upgrade", a)
		}
	}
	for _, a := range []string{"s3:GetObject", "s3:GetObjectTagging", "s3:ListBucket"} {
		if !evalPolicy(t, store, "ReadOnlyAccess", a) {
			t.Errorf("ReadOnlyAccess does not allow %s", a)
		}
	}
	for _, a := range []string{"s3:PutObject", "s3:DeleteObject", "s3:PutObjectTagging"} {
		if evalPolicy(t, store, "ReadOnlyAccess", a) {
			t.Errorf("ReadOnlyAccess allows %s", a)
		}
	}
	if p, _ := store.GetIAMPolicy("FullAccess"); p.Document != edited {
		t.Error("a built-in policy the operator edited was overwritten")
	}
}
