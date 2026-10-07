package s3

import (
	"net/http/httptest"
	"sync"
	"testing"
)

// The dashboard changes the admin pair while requests are authenticated. The
// pair was read and written with no lock, a data race the race detector flags.
func TestAdminCredentialChangeIsRaceFree(t *testing.T) {
	a := NewAuthenticator("admin", "secret", nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				a.UpdateAdminCredentials("admin", "secret-2")
			}
		}()
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/", nil)
			for j := 0; j < 200; j++ {
				a.resolveIdentity("admin", r)
				_ = a.GetAccessKey()
			}
		}()
	}
	wg.Wait()
}
