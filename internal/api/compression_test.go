package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompressResponses(t *testing.T) {
	t.Parallel()

	large := bytes.Repeat([]byte("contract interface "), 200)
	small := []byte(`{"status":"ok"}`)

	tests := []struct {
		name         string
		accept       string
		contentType  string
		body         []byte
		wantEncoding string
	}{
		{
			name:         "large JSON is gzipped",
			accept:       "gzip",
			contentType:  "application/json; charset=utf-8",
			body:         large,
			wantEncoding: "gzip",
		},
		{
			name:         "large TypeScript is gzipped",
			accept:       "gzip, deflate",
			contentType:  "text/plain; charset=utf-8",
			body:         large,
			wantEncoding: "gzip",
		},
		{
			name:        "a client without Accept-Encoding gets the body as-is",
			contentType: "application/json; charset=utf-8",
			body:        large,
		},
		{
			name:        "a small response is not compressed",
			accept:      "gzip",
			contentType: "application/json; charset=utf-8",
			body:        small,
		},
		{
			name:        "an explicit refusal is honoured",
			accept:      "gzip;q=0",
			contentType: "application/json; charset=utf-8",
			body:        large,
		},
		{
			name:        "a type that was not configured is not compressed",
			accept:      "gzip",
			contentType: "image/png",
			body:        large,
		},
		{
			name:        "an event stream is never compressed",
			accept:      "gzip",
			contentType: "text/event-stream",
			body:        large,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := compressResponses("application/json", "text/plain")(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", tt.contentType)
					_, _ = w.Write(tt.body)
				}),
			)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.accept != "" {
				req.Header.Set("Accept-Encoding", tt.accept)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantEncoding, rec.Header().Get("Content-Encoding"))

			body := rec.Body.Bytes()
			if tt.wantEncoding == "gzip" {
				reader, err := gzip.NewReader(bytes.NewReader(body))
				require.NoError(t, err)
				body, err = io.ReadAll(reader)
				require.NoError(t, err)
				require.NoError(t, reader.Close())
			}
			assert.Equal(t, tt.body, body, "the decoded response must match what the handler wrote")
		})
	}
}
