package transport

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/canonical/landscape-client-core/internal/persist"
)

// TransportAttachmentFetcher downloads script attachments from the Landscape server.
// It derives the attachment base URL from the message-system URL by stripping its
// last path segment and replacing it with "attachment/":
//
//	"https://host/message-system" → "https://host/attachment/"
type TransportAttachmentFetcher struct {
	client  *Client
	baseURL string // e.g. "https://host/attachment/"
	store   *persist.Store
}

// NewAttachmentFetcher creates a TransportAttachmentFetcher.
// msgURL is the Landscape message-system URL (cfg.URL).
func NewAttachmentFetcher(client *Client, msgURL string, store *persist.Store) *TransportAttachmentFetcher {
	idx := strings.LastIndex(msgURL, "/")
	baseURL := msgURL[:idx+1] // includes trailing slash, e.g. "https://host/"
	return &TransportAttachmentFetcher{
		client:  client,
		baseURL: baseURL + "attachment/",
		store:   store,
	}
}

// FetchAttachment fetches a single attachment by ID from the Landscape server.
// It authenticates using the secure-id loaded from the persist store.
func (f *TransportAttachmentFetcher) FetchAttachment(ctx context.Context, id int64) ([]byte, error) {
	state, err := f.store.Load()
	if err != nil {
		return nil, fmt.Errorf("attachments: loading state: %w", err)
	}

	attachURL := f.baseURL + strconv.FormatInt(id, 10)
	headers := map[string]string{
		"X-Computer-ID": state.SecureID,
	}
	return f.client.Get(ctx, attachURL, headers)
}
