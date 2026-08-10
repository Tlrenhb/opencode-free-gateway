package server

import (
	"io"
	"net/http"
)

// ioLimitReader wraps r.Body with a 4 MiB cap; the returned reader is
// itself an io.ReadCloser (callers must Close it, like r.Body).
func ioLimitReader(body io.ReadCloser) io.ReadCloser {
	return http.MaxBytesReader(nil, body, 4<<20)
}
