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

// fetch sends the candidate's prepared request: always a GET, never anything
// that could change vendor state.
func fetch(ctx context.Context, client *http.Client, c Candidate) response {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return response{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	for _, h := range c.headers {
		req.Header.Set(h[0], h[1])
	}
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
