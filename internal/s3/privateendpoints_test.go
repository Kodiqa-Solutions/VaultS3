package s3

import "testing"

// The save-time check refused private addresses even when the operator had
// allowed webhooks or functions on their own network, so the option could not
// be used. With the option on only the metadata service stays refused.
func TestEndpointValidationFollowsTheOperatorsChoice(t *testing.T) {
	if validateEndpointURL("http://10.1.2.3/hook", false) == nil {
		t.Error("a private address was accepted with the option off")
	}
	if err := validateEndpointURL("http://10.1.2.3/hook", true); err != nil {
		t.Errorf("a private address was refused with the option on: %v", err)
	}
	if validateEndpointURL("http://169.254.169.254/latest", true) == nil {
		t.Error("the metadata service was accepted with the option on")
	}
}
