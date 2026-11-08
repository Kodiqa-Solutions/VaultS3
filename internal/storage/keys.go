package storage

import "strings"

// KeyProblem reports why key cannot name an object, or "" when it can. Every
// way a key enters (S3, the dashboard, Snowball, migration, replication) uses
// it, so they cannot drift apart.
//
// The engine stores an object at its key under the bucket directory, so a key
// must name exactly one file and never one the engine keeps for itself:
//   - ".." would leave the bucket;
//   - "." and an empty segment (other than a trailing "/" folder marker) are
//     folded away by the path, so "./doc" and "a//doc" named the files of
//     "doc" and "a/doc": a body-carried key (DeleteObjects, a form upload, the
//     dashboard) reached an object its own key was not authorized for, or a
//     locked one, as another key;
//   - a first segment ".vs", ".ec" or ".multipart", and any segment starting
//     ".vaults3-tmp-", are the engine's version store, erasure shards,
//     multipart staging and temporary uploads.
func KeyProblem(key string) string {
	switch {
	case key == "":
		return "Key must not be empty"
	case strings.ContainsRune(key, 0):
		return "Key must not contain a NUL byte"
	case strings.HasPrefix(key, "/"):
		return "Key must not begin with '/'"
	}
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		switch {
		case seg == "..":
			return "Key must not contain '..' path segments"
		case seg == ".":
			return "Key must not contain '.' path segments"
		case seg == "" && i != len(segs)-1:
			return "Key must not contain an empty path segment ('//')"
		case i == 0 && (seg == ".vs" || seg == ".ec" || seg == ".multipart"):
			return "Key must not begin with " + seg + "/, which the server keeps for its own files"
		case strings.HasPrefix(seg, ".vaults3-tmp-"):
			return "Key must not contain a segment beginning with .vaults3-tmp-, which the server keeps for its own files"
		}
	}
	return ""
}
