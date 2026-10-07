package storage

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

// BenchmarkFileSystemPutObject measures what the durability syncs cost. The
// "nosync" mode reproduces the old behaviour (no file or directory sync),
// "filesync" syncs only the file, "full" is the default.
func BenchmarkFileSystemPutObject(b *testing.B) {
	for _, size := range []int{4 << 10, 1 << 20} {
		for _, mode := range []string{"nosync", "filesync", "full"} {
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, mode), func(b *testing.B) {
				origF := syncFile
				defer func() { syncFile = origF }()
				fs, err := NewFileSystem(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				fs.CreateBucketDir("b")
				switch mode {
				case "nosync":
					syncFile = func(*os.File) error { return nil }
					fs.SetSyncDirs(false)
				case "filesync":
					fs.SetSyncDirs(false)
				}
				data := bytes.Repeat([]byte("a"), size)
				b.SetBytes(int64(size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := fs.PutObject("b", fmt.Sprintf("d%d/k%d", i%16, i), bytes.NewReader(data), int64(size)); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
