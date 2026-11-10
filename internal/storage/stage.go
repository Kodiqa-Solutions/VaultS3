package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// StagingDirName is where replica copies wait on disk until they are
// installed. Bucket names cannot start with a dot, so it is never a bucket.
const StagingDirName = ".vaults3-replica-staging"

// StagedCopy is an object copy received from another node and kept on local
// disk until it is installed. A copy used to stream straight into the engine
// while holding the key's lock, so every client write of that key on this
// node waited for the whole network transfer, for good if the sending node
// stalled. Staging first keeps the lock to the local install.
//
// The bytes arrive in the clear (the sender decrypts), so the staged file is
// encrypted with a random key held only in memory: a crash leaves nothing
// readable, and an encrypted bucket's object is never on disk in the clear.
type StagedCopy struct {
	f    *os.File
	key  []byte
	iv   []byte
	Size int64
}

// StageCopy reads r (want bytes, or to EOF when want < 0) into a staged file
// under dataDir.
func StageCopy(dataDir string, r io.Reader, want int64) (*StagedCopy, error) {
	dir := filepath.Join(dataDir, StagingDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "copy-*")
	if err != nil {
		return nil, err
	}
	s := &StagedCopy{f: f, key: make([]byte, 32), iv: make([]byte, aes.BlockSize)}
	if _, err := rand.Read(s.key); err != nil {
		s.Close()
		return nil, err
	}
	if _, err := rand.Read(s.iv); err != nil {
		s.Close()
		return nil, err
	}
	stream, err := s.stream()
	if err != nil {
		s.Close()
		return nil, err
	}
	src := r
	if want >= 0 {
		src = io.LimitReader(r, want)
	}
	n, err := io.Copy(cipher.StreamWriter{S: stream, W: f}, src)
	if err != nil {
		s.Close()
		return nil, err
	}
	if want >= 0 && n != want {
		s.Close()
		return nil, fmt.Errorf("copy ended after %d of %d bytes", n, want)
	}
	s.Size = n
	return s, nil
}

func (s *StagedCopy) stream() (cipher.Stream, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewCTR(block, s.iv), nil
}

// Reader returns the staged bytes from the start, in the clear.
func (s *StagedCopy) Reader() (io.Reader, error) {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	stream, err := s.stream()
	if err != nil {
		return nil, err
	}
	return cipher.StreamReader{S: stream, R: s.f}, nil
}

// Close removes the staged file.
func (s *StagedCopy) Close() {
	if s == nil || s.f == nil {
		return
	}
	name := s.f.Name()
	_ = s.f.Close()
	_ = os.Remove(name)
	s.f = nil
}

// ClearStaging removes copies left by a process that stopped while staging
// them; their keys went with it.
func ClearStaging(dataDir string) {
	_ = os.RemoveAll(filepath.Join(dataDir, StagingDirName))
}
