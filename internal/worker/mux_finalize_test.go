package worker

import (
	"net/http"
	"testing"
)

func TestHttpError(t *testing.T) {
	e := &httpError{StatusCode: 404}
	if e.Error() != http.StatusText(404) {
		t.Errorf("httpError{404}.Error() = %q, want %q", e.Error(), http.StatusText(404))
	}

	e500 := &httpError{StatusCode: 500}
	if e500.Error() != http.StatusText(500) {
		t.Errorf("httpError{500}.Error() = %q, want %q", e500.Error(), http.StatusText(500))
	}
}
