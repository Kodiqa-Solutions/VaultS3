package migrate

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// A caller-supplied migration endpoint is a server-side request primitive. It
// must not be able to reach loopback, the internal network, or the cloud
// metadata service, where the reply is instance credentials (security
// assessment finding 6).
func TestBlockedDestinations(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"10.0.0.5", "172.16.4.1", "192.168.1.10",
		"169.254.169.254", // AWS/GCP/Azure instance metadata
		"fd00:ec2::254",   // AWS IMDS over IPv6
		"0.0.0.0", "224.0.0.1",
		"::ffff:127.0.0.1", // loopback smuggled through an IPv4-mapped address
		"::ffff:10.1.2.3",
	}
	for _, addr := range blocked {
		if !blockedIP(net.ParseIP(addr)) {
			t.Errorf("%s is reachable as a migration source, it must not be", addr)
		}
	}
	allowed := []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"}
	for _, addr := range allowed {
		if blockedIP(net.ParseIP(addr)) {
			t.Errorf("%s is blocked, but a public source is a legitimate migration", addr)
		}
	}
}

// Only http and https, and only with a host: anything else is a way to reach
// something that is not an S3 endpoint at all.
func TestValidateEndpointRejectsOtherSchemes(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "gopher://x/", "ftp://host/", "http://"} {
		if err := ValidateEndpoint(bad); err == nil {
			t.Errorf("ValidateEndpoint(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"https://s3.example.com", "http://minio.internal:9000/"} {
		if err := ValidateEndpoint(good); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want nil", good, err)
		}
	}
}

// Regression (issue found 2026-11-08): nothing ever allowed private sources,
// so a migration from a MinIO on the same Docker network, the LAN or
// localhost was refused on every server since 4.4.56, with only "could not
// connect". By default it is still refused, now with an error naming the
// setting that allows it.
func TestPrivateSourceIsRefusedByDefaultWithTheSettingNamed(t *testing.T) {
	endpoint := stubS3(t, map[string][]byte{"b1/x": []byte("1")}) // httptest is on loopback
	m := NewManager(nil, nil)
	if _, err := m.TestConnection(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"}); !errors.Is(err, ErrPrivateSource) {
		t.Fatalf("TestConnection to a loopback source: %v, want ErrPrivateSource", err)
	}
	if _, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"}); !errors.Is(err, ErrPrivateSource) {
		t.Fatalf("Start from a loopback source: %v, want ErrPrivateSource", err)
	}
	if !strings.Contains(ErrPrivateSource.Error(), "VAULTS3_ALLOW_PRIVATE_MIGRATION_SOURCES") ||
		!strings.Contains(ErrPrivateSource.Error(), "migration.allow_private_sources") {
		t.Fatalf("the error does not name the setting: %q", ErrPrivateSource)
	}
	m.AllowPrivateSources(true)
	if _, err := m.TestConnection(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"}); err != nil {
		t.Fatalf("with private sources allowed: %v", err)
	}
}

// Allowing private sources opens loopback and the private ranges, never the
// cloud metadata service (link-local, or the private IPv6 IMDS address) or
// addresses no S3 server is reached at.
func TestAllowingPrivateSourcesStillRefusesMetadataAddresses(t *testing.T) {
	dial := func(control func(string, string, syscall.RawConn) error, ip string) error {
		return control("tcp", net.JoinHostPort(ip, "9000"), nil)
	}
	for _, ip := range []string{"127.0.0.1", "::1", "10.0.0.5", "172.17.0.2", "192.168.1.10", "::ffff:10.1.2.3", "8.8.8.8"} {
		if err := dial(privateAllowedControl, ip); err != nil {
			t.Errorf("private sources allowed, %s refused: %v", ip, err)
		}
	}
	for _, ip := range []string{"169.254.169.254", "169.254.1.1", "fe80::1", "fd00:ec2::254", "::ffff:169.254.169.254", "0.0.0.0", "224.0.0.1"} {
		err := dial(privateAllowedControl, ip)
		if err == nil {
			t.Errorf("private sources allowed, %s reachable: it must never be", ip)
		} else if errors.Is(err, ErrPrivateSource) {
			t.Errorf("%s: the error suggests allowing private sources, which would not help", ip)
		}
		if err := dial(guardedControl, ip); err == nil || errors.Is(err, ErrPrivateSource) {
			t.Errorf("default guard, %s: %v, want a refusal that does not point at the setting", ip, err)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.10", "::ffff:10.1.2.3"} {
		if err := dial(guardedControl, ip); !errors.Is(err, ErrPrivateSource) {
			t.Errorf("default guard, %s: %v, want ErrPrivateSource", ip, err)
		}
	}
	if err := dial(guardedControl, "8.8.8.8"); err != nil {
		t.Errorf("default guard refused a public source: %v", err)
	}
}

// A failed source request reaches the dashboard (the start request and the
// job list), so it must not carry what the source replied: with private
// sources allowed that would read internal services (security assessment
// finding 6).
func TestSourceErrorDoesNotCarryTheReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "INTERNAL-SERVICE-SECRET")
	}))
	t.Cleanup(srv.Close)
	m := NewManager(nil, nil)
	m.AllowPrivateSources(true)
	_, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	if err == nil {
		t.Fatal("Start against a 403 source succeeded")
	}
	if strings.Contains(err.Error(), "INTERNAL-SERVICE-SECRET") {
		t.Fatalf("the error carries the source's reply: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("the error lost the status: %v", err)
	}
}

// Regression (review of the private-source fix): the migration kept its own
// copy of the address guard, which had drifted from the shared one and let
// carrier-grade NAT space through, Alibaba Cloud's metadata service among
// it. NAT64 and 6to4 addresses carrying an internal IPv4 address are refused
// as that address.
func TestCGNATAndEmbeddedInternalAddressesAreRefused(t *testing.T) {
	dial := func(control func(string, string, syscall.RawConn) error, ip string) error {
		return control("tcp", net.JoinHostPort(ip, "9000"), nil)
	}
	for _, ip := range []string{"100.64.0.1", "100.100.100.200", "::ffff:100.100.100.200",
		"64:ff9b::7f00:1", "64:ff9b::a00:5", "64:ff9b::a9fe:a9fe", "2002:7f00:1::1", "2002:a9fe:a9fe::1", "fec0::1"} {
		if dial(guardedControl, ip) == nil {
			t.Errorf("default guard lets %s through", ip)
		}
	}
	// Allowing private sources never opens a metadata service, however it is
	// written, or multicast.
	for _, ip := range []string{"100.100.100.200", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1", "239.1.1.1"} {
		if dial(privateAllowedControl, ip) == nil {
			t.Errorf("private sources allowed, %s reachable", ip)
		}
	}
	// A public address written as NAT64 is still a public source.
	if err := dial(guardedControl, "64:ff9b::808:808"); err != nil {
		t.Errorf("NAT64 form of 8.8.8.8 refused: %v", err)
	}
}

// Regression (review of the private-source fix): an error reaching the
// dashboard quoted the source (Go's XML errors quote its tokens) or told a
// closed port from an open one, which with private sources allowed reads or
// maps the internal network. The start error and the job's error say only
// what PublicSourceError allows. Mistakes in the request itself still show.
func TestMigrationErrorsNeverQuoteTheSource(t *testing.T) {
	const secret = "INTERNAL_SECRET_TOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<x>&"+secret+";</x>") // 200 with a reply that does not parse
	}))
	t.Cleanup(srv.Close)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true)

	_, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	if err == nil {
		t.Fatal("Start against a source answering garbage succeeded")
	}
	if msg := PublicSourceError(err); strings.Contains(msg, secret) {
		t.Fatalf("the start error shown quotes the source: %q", msg)
	}
	_, closed := m.Start(StartConfig{Endpoint: "http://127.0.0.1:1", AccessKey: "k", SecretKey: "s"})
	if closed == nil || PublicSourceError(closed) != PublicSourceError(err) {
		t.Fatalf("a closed port (%q) is told apart from a garbage reply (%q)", PublicSourceError(closed), PublicSourceError(err))
	}
	// Nor from a port that answers HTTP with anything but a refused key
	// (second review): the status told which internal ports run a service.
	status := func(code int, body string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, body)
		}))
		defer s.Close()
		_, err := m.Start(StartConfig{Endpoint: s.URL, AccessKey: "k", SecretKey: "s"})
		return PublicSourceError(err)
	}
	if got := status(http.StatusNotFound, ""); got != PublicSourceError(closed) {
		t.Fatalf("an HTTP 404 port (%q) is told apart from a closed one (%q)", got, PublicSourceError(closed))
	}
	// Nor a 401 from a service that is not S3 (fifth review): an internal port
	// running anything behind authentication showed as open.
	if got := status(http.StatusUnauthorized, `{"message":"Unauthorized"}`); got != PublicSourceError(closed) {
		t.Fatalf("a non-S3 401 (%q) is told apart from a closed port (%q)", got, PublicSourceError(closed))
	}
	for _, code := range []string{"InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied"} {
		reply := `<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>no</Message></Error>`
		if got := status(http.StatusForbidden, reply); !strings.Contains(got, "refused the access key") {
			t.Fatalf("an S3 server refusing the key with %s reads %q, want it said", code, got)
		}
	}

	// The job's error, when listing a bucket fails after the job started.
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"b"}})
	if err != nil {
		t.Fatalf("Start with a named bucket: %v", err)
	}
	if j := waitDone(t, m, id); j.Status != "failed" || strings.Contains(j.Error, secret) {
		t.Fatalf("job %s, error %q: want failed without the source's reply", j.Status, j.Error)
	}

	var re *RequestError
	if _, err := m.Start(StartConfig{Endpoint: "ftp://x/"}); !errors.As(err, &re) || !strings.Contains(re.Msg, "ftp") {
		t.Fatalf("a bad endpoint is no longer reported as such: %v", err)
	}
}

// Regression (second review of the private-source fix): the migration client
// followed redirects, and the dial check only sees each address in turn, so a
// source could answer an object GET with a redirect to an internal service
// and have its reply stored as the object (with private sources allowed, any
// internal address). Redirects are not followed.
func TestMigrationDoesNotFollowRedirects(t *testing.T) {
	const secret = "INTERNAL_SECRET_BODY"
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, secret)
	}))
	t.Cleanup(internal.Close)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
		case r.URL.Query().Get("list-type") == "2":
			io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>loot</Key><Size>20</Size></Contents></ListBucketResult>`)
		case r.URL.Path == "/b/loot":
			http.Redirect(w, r, internal.URL+"/admin/secret", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(src.Close)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true)
	id, err := m.Start(StartConfig{Endpoint: src.URL, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, m, id)
	if rc, _, err := eng.GetObject("b", "loot"); err == nil {
		data, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(data), secret) {
			t.Fatalf("a redirect stored the internal service's reply as the object: %q", data)
		}
	}
}

// Regression (third review of the private-source fix): whatever answered 200
// at {endpoint}/{bucket}?policy was stored as the bucket's policy and read
// back through GetBucketPolicy, so with private sources allowed a migration
// read any internal HTTP service. Only a policy document is stored now.
func TestMigrationStoresOnlyARealPolicyDocument(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		kept  bool
	}{
		"internal service JSON":       {`{"internal_secret":"db-password-hunter2"}`, false},
		"statement without an effect": {`{"Statement":[{"Action":"s3:GetObject"}]}`, false},
		"real policy":                 {`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::probe/*"}]}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Has("policy") {
					io.WriteString(w, tc.reply)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(svc.Close)
			store, eng := newLocal(t)
			m := NewManager(store, eng)
			m.AllowPrivateSources(true)
			id, err := m.Start(StartConfig{Endpoint: svc.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"probe"}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			j := waitDone(t, m, id)
			got, _ := store.GetBucketPolicy("probe")
			if stored := len(got) > 0; stored != tc.kept {
				t.Fatalf("policy stored = %v (%q), want %v", stored, got, tc.kept)
			}
			if (j.Policies == 1) != tc.kept {
				t.Fatalf("policies counter %d, want it to match what was stored", j.Policies)
			}
		})
	}
}

// Regression (fourth review): any 200 that was well-formed XML parsed as an
// empty S3 reply, so a job against an internal service that answers XML
// "completed" where one answering JSON failed, which told them apart. Replies
// need the S3 root element, so such a service is a failed source.
func TestNonS3XMLServiceIsAFailedSource(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<status><Contents><Key>x</Key></Contents></status>")
	}))
	t.Cleanup(svc.Close)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true)
	id, err := m.Start(StartConfig{Endpoint: svc.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"b"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if j := waitDone(t, m, id); j.Status != "failed" || j.Total != 0 {
		t.Fatalf("job against a non-S3 XML service: status %s, total %d, want failed with nothing listed", j.Status, j.Total)
	}
	if _, err := m.TestConnection(StartConfig{Endpoint: svc.URL, AccessKey: "k", SecretKey: "s"}); err == nil {
		t.Fatal("the connection test accepted a non-S3 XML reply")
	}
}

// Allowing private sources still installs a dial check, the one that refuses
// the metadata service (fourth review: replacing it with none passed).
func TestPrivateSourceClientStillRefusesMetadata(t *testing.T) {
	src := newSourceAllowingPrivate("http://169.254.169.254", "k", "s", "us-east-1", 5)
	if _, err := src.ListBuckets(); err == nil || !strings.Contains(err.Error(), "blocked destination") {
		t.Fatalf("private sources allowed, dialing the metadata service: %v, want a blocked destination", err)
	}
}

func TestIsBucketPolicyNeedsAnEffectAndAnAction(t *testing.T) {
	for doc, want := range map[string]bool{
		`{"Statement":{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}}`: true,
		`{"Statement":[{"Effect":"Deny","NotAction":"s3:*","Resource":"*"}]}`:     true,
		`{"Statement":[{"Effect":"Allow","Resource":"*"}]}`:                       false,
		`{"Statement":[{"Effect":"Maybe","Action":"s3:*"}]}`:                      false,
		`{"Statement":[]}`: false,
		`{"status":"ok"}`:  false,
		`<xml/>`:           false,
	} {
		if got := isBucketPolicy([]byte(doc)); got != want {
			t.Errorf("isBucketPolicy(%s) = %v, want %v", doc, got, want)
		}
	}
}
