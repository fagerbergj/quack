package memory

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// MemoryPageMaxLimit bounds a listMemories request regardless of what a
// caller requests, mirroring store.ChatsPageMaxLimit.
const MemoryPageMaxLimit = 200

// ErrInvalidPageToken: the token doesn't decode, or was issued under a different bucket filter or sort.
var ErrInvalidPageToken = errors.New("memory: invalid page token")

// pageToken is listMemories' offset anchor, bound to the bucket filter and sort it was issued under:
// an offset under one sort names a different row under another, so replay must fail loudly.
type pageToken struct {
	Sort   string `json:"s"`
	Bucket string `json:"b"`
	Offset int    `json:"o"`
}

// EncodePageToken builds the opaque continuation token for the next page
// starting at offset, issued under the given bucket filter and sort.
func EncodePageToken(bucket, sort string, offset int) string {
	b, _ := json.Marshal(pageToken{Sort: sort, Bucket: bucket, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodePageToken validates s was issued under bucket and sort, and returns its offset.
func DecodePageToken(s, bucket, sort string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidPageToken, err)
	}
	var t pageToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidPageToken, err)
	}
	if t.Sort != sort {
		return 0, fmt.Errorf("%w: issued for sort %q, not %q", ErrInvalidPageToken, t.Sort, sort)
	}
	if t.Bucket != bucket {
		return 0, fmt.Errorf("%w: issued for bucket %q, not %q", ErrInvalidPageToken, t.Bucket, bucket)
	}
	if t.Offset < 0 {
		return 0, fmt.Errorf("%w: negative offset", ErrInvalidPageToken)
	}
	return t.Offset, nil
}
