package transport

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/canonical/landscape-client-core/internal/persist"
)

// AttachmentFetcher downloads script attachments from the Landscape server.
// It derives the attachment base URL from the message-system URL by stripping its
// last path segment and replacing it with "attachment/":
//
//	"https://host/message-system" → "https://host/attachment/"
type AttachmentFetcher struct {
	client  *Client
	baseURL string // e.g. "https://host/attachment/"
	store   *persist.Store
}

// NewAttachmentFetcher creates an AttachmentFetcher.
// msgURL is the Landscape message-system URL (cfg.URL).
func NewAttachmentFetcher(client *Client, msgURL string, store *persist.Store) *AttachmentFetcher {
	idx := strings.LastIndex(msgURL, "/")
	baseURL := msgURL[:idx+1] // includes trailing slash, e.g. "https://host/"
	return &AttachmentFetcher{
		client:  client,
		baseURL: baseURL + "attachment/",
		store:   store,
	}
}

// FetchAttachment fetches a single attachment by ID from the Landscape server.
// It authenticates using the secure-id loaded from the persist store.
func (f *AttachmentFetcher) FetchAttachment(ctx context.Context, id int64) ([]byte, error) {
	state, err := f.store.Load()
	if err != nil {
		return nil, fmt.Errorf("attachments: cannot load state: %w", err)
	}

	attachURL := f.baseURL + strconv.FormatInt(id, 10)
	headers := map[string]string{
		"X-Computer-ID": state.SecureID,
	}
	return f.client.Get(ctx, attachURL, headers)
}
