package storage

// VersionIDProblem says why a version ID from a request cannot name a
// version, or "" when it can: 1 to 128 letters, digits, '-', '_' or '.',
// and not "." or "..". Every ID this server has issued fits ("null", and
// the time-ordered hex IDs).
//
// A version is a file under its key's own version folder, named by its ID,
// and the ID was never checked: "secret/<id>" on key "doc" named the file of
// version <id> of the key "doc/secret", so a delete of a version that does not
// exist (and so is under no lock) removed another key's version, past its
// COMPLIANCE retention.
func VersionIDProblem(id string) string {
	if id == "" || len(id) > 128 {
		return "invalid version id"
	}
	if id == "." || id == ".." {
		return "invalid version id"
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.'
		if !ok {
			return "invalid version id"
		}
	}
	return ""
}
