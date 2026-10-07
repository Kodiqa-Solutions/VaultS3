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
	// SessionScoped marks a temporary credential minted with a session policy.
	// Such a session may do only what BOTH its session policy and its source
	// user's policies allow, as in AWS: the session policy narrows the user, it
	// never widens them. Policies then holds the source user's set and
	// SessionPolicies the session's own.
	SessionScoped   bool
	SessionPolicies []Policy
}

// EvaluateIdentity decides one request for an identity. For a scoped session
// the answer is the intersection of the two policy sets: allowed only when both
// allow, and a Deny in either is an explicit Deny.
func EvaluateIdentity(id *Identity, action, resource string, ctx map[string]string) (allowed, explicitDeny bool) {
	allowed, explicitDeny = EvaluateDetailed(id.Policies, action, resource, ctx)
	if !id.SessionScoped {
		return allowed, explicitDeny
	}
	sAllowed, sDeny := EvaluateDetailed(id.SessionPolicies, action, resource, ctx)
	if explicitDeny || sDeny {
		return false, true
	}
	return allowed && sAllowed, false
}
