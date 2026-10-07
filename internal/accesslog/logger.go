package accesslog

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type AccessEntry struct {
	Time     time.Time `json:"time"`
	Method   string    `json:"method"`
	Bucket   string    `json:"bucket"`
	Key      string    `json:"key,omitempty"`
	Status   int       `json:"status"`
	Bytes    int64     `json:"bytes"`
	ClientIP string    `json:"client_ip"`
}

type AccessLogger struct {
	file *os.File
	enc  *json.Encoder
	mu   sync.Mutex
}

func NewAccessLogger(path string) (*AccessLogger, error) {
	// 0600: the log records client addresses, access keys and object keys.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &AccessLogger{
		file: f,
		enc:  json.NewEncoder(f),
	}, nil
}

func (l *AccessLogger) Log(entry AccessEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(entry)
}

func (l *AccessLogger) Close() error {
	return l.file.Close()
}
