package transport_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

func TestTransportAttachmentFetcher_Success(t *testing.T) {
	var gotComputerID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotComputerID = r.Header.Get("X-Computer-ID")
		if r.URL.Path != "/attachment/42" {
			t.Errorf("path = %q, want /attachment/42", r.URL.Path)
		}
		_, _ = w.Write([]byte("file-contents"))
	}))
	defer srv.Close()

	msgURL := srv.URL + "/message-system"

	store := persist.New(t.TempDir() + "/state.json")
	st, _ := store.Load()
	st.SecureID = "my-secure-id"
	if err := store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	tc, _ := transport.New(transport.Config{})
	fetcher := transport.NewAttachmentFetcher(tc, msgURL, store)

	data, err := fetcher.FetchAttachment(context.Background(), 42)
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if string(data) != "file-contents" {
		t.Errorf("data = %q, want %q", data, "file-contents")
	}
	if gotComputerID != "my-secure-id" {
		t.Errorf("X-Computer-ID = %q, want %q", gotComputerID, "my-secure-id")
	}
}

func TestTransportAttachmentFetcher_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := persist.New(t.TempDir() + "/state.json")
	st, _ := store.Load()
	st.SecureID = "id"
	_ = store.Save(st)

	tc, _ := transport.New(transport.Config{})
	fetcher := transport.NewAttachmentFetcher(tc, srv.URL+"/message-system", store)

	_, err := fetcher.FetchAttachment(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestTransportAttachmentFetcher_URLConstruction(t *testing.T) {
	tests := []struct {
		msgSuffix  string
		id         int64
		wantSuffix string
	}{
		{"/message-system", 5, "/attachment/5"},
		{"/landscape/message-system", 99, "/landscape/attachment/99"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("id=%d", tt.id), func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = w.Write([]byte("ok"))
			}))
			defer srv.Close()

			store := persist.New(t.TempDir() + "/state.json")
			st, _ := store.Load()
			st.SecureID = "id"
			_ = store.Save(st)

			tc, _ := transport.New(transport.Config{})
			fetcher := transport.NewAttachmentFetcher(tc, srv.URL+tt.msgSuffix, store)
			_, _ = fetcher.FetchAttachment(context.Background(), tt.id)

			if gotPath != tt.wantSuffix {
				t.Errorf("path = %q, want %q", gotPath, tt.wantSuffix)
			}
		})
	}
}
