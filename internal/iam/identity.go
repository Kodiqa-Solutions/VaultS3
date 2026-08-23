package iam

// Identity represents an authenticated caller.
type Identity struct {
	AccessKey string
	UserID    string
	IsAdmin   bool
	Policies  []Policy
	// PolicyLoadFailed is set when one of this identity's attached policies could
	// not be parsed. An unreadable policy is not an absent one: skipping it drops
	// whatever it said, and a dropped Deny silently widens access when another
	// policy allows. Authorization refuses outright rather than deciding on a
	// partial policy set, which is what the console path already did.
	PolicyLoadFailed bool
	AllowedCIDRs     []string
}
