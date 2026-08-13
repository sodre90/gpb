//go:build live

package gphotos

import (
	"testing"
	"time"
)

// TestLiveTimelinePaging proves the library listing still walks. Every photo that is in no album
// is reachable only this way, and the walk is only sound if each page continues where the last
// one stopped — a cursor that silently restarts would back up the newest 300 items forever.
//
// It prints counts and dates, never a media key.
func TestLiveTimelinePaging(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	token, previous, total := "", time.Time{}, 0
	for page := range 3 {
		listing, err := client.Timeline(ctx, token)
		if err != nil {
			t.Fatalf("page %d: %v", page+1, err)
		}
		if len(listing.Items) == 0 {
			t.Fatalf("page %d carried no items", page+1)
		}

		newest := listing.Items[0].CapturedAt
		oldest := listing.Items[len(listing.Items)-1].CapturedAt
		t.Logf("page %d: %d items, %s .. %s, continues %v",
			page+1, len(listing.Items), newest.Format(time.DateOnly), oldest.Format(time.DateOnly),
			listing.NextToken != "")

		if !previous.IsZero() && newest.After(previous) {
			t.Fatalf("page %d starts at %s, which is newer than where page %d stopped",
				page+1, newest, page)
		}
		previous, total = oldest, total+len(listing.Items)

		if listing.NextToken == "" {
			t.Logf("the library ended after %d items", total)
			return
		}
		token = listing.NextToken
	}
	t.Logf("three pages carried %d items", total)
}
