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

// ErrInvalidPageToken is returned by DecodePageToken when the token doesn't
// decode, or was issued under a different bucket filter than it's being
// replayed against.
var ErrInvalidPageToken = errors.New("memory: invalid page token")

const pageSortRecencyDesc = "recency_desc"

// pageToken is listMemories' opaque continuation token: an offset anchor
// (the store's list is already a full scan sorted newest-first, sliced by
// offset - see qdrantIndex.list/sqliteIndex.list) bound to the bucket filter
// it was issued under, the same way store.chatsPageToken binds to scope.
type pageToken struct {
	Sort   string `json:"s"`
	Bucket string `json:"b"`
	Offset int    `json:"o"`
}

// EncodePageToken builds the opaque continuation token for the next page
// starting at offset, issued under the given bucket filter.
func EncodePageToken(bucket string, offset int) string {
	b, _ := json.Marshal(pageToken{Sort: pageSortRecencyDesc, Bucket: bucket, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodePageToken validates s was issued under bucket and returns its offset.
func DecodePageToken(s, bucket string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	var t pageToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidPageToken, err)
	}
	if t.Sort != pageSortRecencyDesc {
		return 0, fmt.Errorf("%w: issued for sort %q, not %q", ErrInvalidPageToken, t.Sort, pageSortRecencyDesc)
	}
	if t.Bucket != bucket {
		return 0, fmt.Errorf("%w: issued for bucket %q, not %q", ErrInvalidPageToken, t.Bucket, bucket)
	}
	if t.Offset < 0 {
		return 0, fmt.Errorf("%w: negative offset", ErrInvalidPageToken)
	}
	return t.Offset, nil
}
