//go:build live

package main

import (
	"context"
	"testing"
	"time"
)

// Public catalog only: never read credentials or authenticate in live tests.
func TestLivePublicCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c := newClient("", "")
	result, err := c.Search(ctx, "heim", 1)
	if err != nil {
		t.Fatal("public search failed")
	}
	if result == nil {
		t.Fatal("missing search result")
	}
	page, err := c.page(ctx, "629592", false)
	if err != nil {
		t.Fatal("public book details failed")
	}
	if page.Authenticated || len(page.result().Ebooks) == 0 {
		t.Fatal("unexpected public book result")
	}
	t.Log("Public search and e-book resolution succeeded; no authentication or writes performed")
}
