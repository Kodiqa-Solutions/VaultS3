// Command signrelease signs a release's checksums.txt with the Ed25519 release
// key, so vaults3 self-update can verify who published a release and not only
// that the download is intact.
//
//	RELEASE_SIGNING_KEY=<base64 private key> go run ./scripts/signrelease checksums.txt
//	go run ./scripts/signrelease -genkey   # prints a new key pair
//
// It writes checksums.txt.sig, the base64 signature over the file's bytes.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-genkey" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err)
		}
		fmt.Println("public  (build into the binary, safe to publish):", base64.StdEncoding.EncodeToString(pub))
		fmt.Println("private (RELEASE_SIGNING_KEY secret, never commit):", base64.StdEncoding.EncodeToString(priv))
		return
	}
	if len(os.Args) != 2 {
		fail(fmt.Errorf("usage: signrelease <checksums.txt> | -genkey"))
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("RELEASE_SIGNING_KEY"))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		fail(fmt.Errorf("RELEASE_SIGNING_KEY must be a base64 Ed25519 private key"))
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(key), data)
	if err := os.WriteFile(os.Args[1]+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "signrelease:", err)
	os.Exit(1)
}
