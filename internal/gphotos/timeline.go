package gphotos

import "context"

const timelineRPC = "lcxiM"

// A timeline page is flatter than an album's: the entries come first and nothing names them,
// because the library is not a thing with a title.
const (
	timelineEntriesIndex = 0
	timelineCursorIndex  = 1
)

// Timeline returns one page of the library — everything the account holds, newest capture first,
// including the photos that are in no album and so appear nowhere else in this client. Google
// pages these at 300 entries; pass the previous NextToken to continue.
//
// The six arguments after the cursor are what photos.google.com sends its own timeline, measured
// 2026-08-10: two flags it always sets to 1, and time bounds it leaves empty to mean "from the
// newest". They are passed through unchanged rather than understood, because a listing that
// works is worth more than a tidy signature.
func (c *Client) Timeline(ctx context.Context, pageToken string) (ItemPage, error) {
	payload, err := c.call(ctx, timelineRPC,
		[]any{nullable(pageToken), nil, nil, nil, 1, 1, nil}, "/")
	if err != nil {
		return ItemPage{}, err
	}
	return decodeTimelinePage(payload)
}

func decodeTimelinePage(payload any) (ItemPage, error) {
	root := rootOf(payload, timelineRPC)

	items, err := decodeMediaItems(root.at(timelineEntriesIndex))
	if err != nil {
		return ItemPage{}, err
	}

	page := ItemPage{Items: items}
	if page.NextToken, err = root.at(timelineCursorIndex).cursor(); err != nil {
		return ItemPage{}, err
	}
	return page, nil
}
