package proxy

import (
	"errors"
	"fmt"
	"net/url"
)

// SafeLogURL omits credentials, query parameters and fragments from diagnostics.
func SafeLogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// SafeLogError removes URLs embedded in HTTP client errors.
func SafeLogError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s %s: %v", urlErr.Op, SafeLogURL(urlErr.URL), urlErr.Err)
	}
	return err
}
