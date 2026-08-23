package erasure

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

// Encoder handles Reed-Solomon encoding and decoding.
type Encoder struct {
	rs           reedsolomon.Encoder
	dataShards   int
	parityShards int
}

// NewEncoder creates a new Reed-Solomon encoder.
func NewEncoder(dataShards, parityShards int) (*Encoder, error) {
	rs, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil, fmt.Errorf("create reed-solomon encoder: %w", err)
	}
	return &Encoder{
		rs:           rs,
		dataShards:   dataShards,
		parityShards: parityShards,
	}, nil
}

// EncodeStripe fills the parity entries of an already-split stripe in place. The
// caller supplies dataShards+parityShards equal-length slices with the data
// entries populated, which is what lets parity be generated one aligned stripe at
// a time instead of from a copy of the whole object.
func (e *Encoder) EncodeStripe(shards [][]byte) error {
	if len(shards) != e.dataShards+e.parityShards {
		return fmt.Errorf("expected %d shards, got %d", e.dataShards+e.parityShards, len(shards))
	}
	return e.rs.Encode(shards)
}

// Encode splits data into data+parity shards using Reed-Solomon encoding.
// Returns a slice of shard byte slices (len = dataShards + parityShards).
func (e *Encoder) Encode(data []byte) ([][]byte, error) {
	// Split into data shards (pads the last shard if needed)
	shards, err := e.rs.Split(data)
	if err != nil {
		return nil, fmt.Errorf("split data: %w", err)
	}

	// Generate parity shards
	if err := e.rs.Encode(shards); err != nil {
		return nil, fmt.Errorf("encode parity: %w", err)
	}

	return shards, nil
}

// Decode reconstructs the original data from shards.
// Missing shards should be set to nil. At least dataShards intact shards are required.
func (e *Encoder) Decode(shards [][]byte, originalSize int64) ([]byte, error) {
	// Verify we have the right number of shards
	if len(shards) != e.dataShards+e.parityShards {
		return nil, fmt.Errorf("expected %d shards, got %d", e.dataShards+e.parityShards, len(shards))
	}

	// Check if reconstruction is needed
	needsReconstruct := false
	for _, s := range shards {
		if s == nil {
			needsReconstruct = true
			break
		}
	}

	if needsReconstruct {
		if err := e.rs.Reconstruct(shards); err != nil {
			return nil, fmt.Errorf("reconstruct: %w", err)
		}
	}

	// Verify integrity
	ok, err := e.rs.Verify(shards)
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("shard verification failed")
	}

	// Join data shards and trim to original size
	var buf bytes.Buffer
	buf.Grow(int(originalSize))
	if err := e.rs.Join(io.Writer(&buf), shards, int(originalSize)); err != nil {
		return nil, fmt.Errorf("join shards: %w", err)
	}

	return buf.Bytes(), nil
}

// Verify checks if all shards are consistent.
func (e *Encoder) Verify(shards [][]byte) (bool, error) {
	return e.rs.Verify(shards)
}

// Reconstruct rebuilds missing shards (set nil for missing).
func (e *Encoder) Reconstruct(shards [][]byte) error {
	return e.rs.Reconstruct(shards)
}
