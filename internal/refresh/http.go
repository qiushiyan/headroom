package refresh

import (
	"context"
	"io"
	"net/http"
)

// response is one account's fetch outcome; Err covers transport failures.
type response struct {
	StatusCode int
	Body       []byte
	Err        error
}

func fetch(ctx context.Context, client *http.Client, url, token string) response {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return response{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return response{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return response{Err: err}
	}
	return response{StatusCode: resp.StatusCode, Body: body}
}
