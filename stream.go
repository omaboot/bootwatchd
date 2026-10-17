package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"io"
)

// The protocol's requests (README.md).
const (
	echoPath   = "/v1/echo"   // GET ?n=<32 hex>: the nonce back, as text
	streamPath = "/v1/stream" // GET ?seed=<32 hex>&n=<bytes>: the seed's stream, its first n bytes
	sinkPath   = "/v1/sink"   // POST ?seed=<32 hex> + body: {"n": bytes read, "match": leading bytes that matched}
	healthPath = "/v1/health" // GET: the counters, for monitoring
)

// Size limits.
const (
	maxStream = 1 << 20   // the most one stream request may ask for
	maxSink   = 256 << 10 // the largest body the sink takes
)

// seed is a stream's key, or an echo's nonce: 16 bytes the client picks.
type seed [16]byte

func (s seed) String() string { return hex.EncodeToString(s[:]) }

// parseSeed reads exactly 32 hex digits.
func parseSeed(h string) (seed, error) {
	var s seed
	if len(h) != 32 {
		return s, errors.New("want 32 hex digits")
	}
	if _, err := hex.Decode(s[:], []byte(h)); err != nil {
		return s, errors.New("want 32 hex digits")
	}
	return s, nil
}

// stream is the seed's stream, endless: AES-128-CTR with the seed as the key, a zero counter
// block and zero plaintext.
func stream(s seed) io.Reader {
	b, _ := aes.NewCipher(s[:]) // 16 bytes: AES-128, never an error
	return &ctrStream{ctr: cipher.NewCTR(b, make([]byte, aes.BlockSize))}
}

type ctrStream struct{ ctr cipher.Stream }

func (s *ctrStream) Read(p []byte) (int, error) {
	clear(p)
	s.ctr.XORKeyStream(p, p)
	return len(p), nil
}

// match reads r to its end and compares it with the seed's stream: n is every byte read, ok the
// leading bytes that equal the stream (after the first wrong byte nothing more matches).
func match(r io.Reader, s seed) (n, ok int64, err error) {
	want := stream(s)
	buf, exp := make([]byte, 32<<10), make([]byte, 32<<10)
	same := true
	for {
		k, rerr := r.Read(buf)
		if k > 0 {
			if same {
				want.Read(exp[:k])
				i := 0
				for i < k && buf[i] == exp[i] {
					i++
				}
				ok += int64(i)
				same = i == k
			}
			n += int64(k)
		}
		if rerr == io.EOF {
			return n, ok, nil
		}
		if rerr != nil {
			return n, ok, rerr
		}
	}
}
