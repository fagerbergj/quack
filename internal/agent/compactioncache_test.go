package agent

import (
	"strconv"
	"testing"
)

func TestBoundedCacheGetPut(t *testing.T) {
	c := newBoundedCache(2)
	if _, ok := c.get("a"); ok {
		t.Fatal("empty cache should miss")
	}
	c.put("a", "1")
	if v, ok := c.get("a"); !ok || v != "1" {
		t.Fatalf("got %q,%v want 1,true", v, ok)
	}
	// Duplicate put is a no-op (keeps first value, no double key).
	c.put("a", "2")
	if v, _ := c.get("a"); v != "1" {
		t.Errorf("duplicate put changed value to %q", v)
	}
}

func TestBoundedCacheFIFOEviction(t *testing.T) {
	c := newBoundedCache(2)
	c.put("a", "1")
	c.put("b", "2")
	c.put("c", "3") // evicts "a" (oldest)
	if _, ok := c.get("a"); ok {
		t.Error("a should have been evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("b should still be present")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("c should be present")
	}
	// Never exceeds max.
	for i := 0; i < 100; i++ {
		c.put("k"+strconv.Itoa(i), "v")
	}
	if len(c.m) > c.max {
		t.Errorf("cache size %d exceeds max %d", len(c.m), c.max)
	}
}

func TestChunkKeyStableAndDistinct(t *testing.T) {
	if chunkKey("hello") != chunkKey("hello") {
		t.Error("same content must hash to same key")
	}
	if chunkKey("hello") == chunkKey("world") {
		t.Error("different content must hash differently")
	}
}
