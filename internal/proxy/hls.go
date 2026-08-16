package proxy

import (
	"bufio"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	// uriAttrRegex matches URI="..." or URI=something inside HLS tags
	uriAttrRegex = regexp.MustCompile(`URI="([^"]+)"|URI=([^,\s]+)`)
)

// RewriteM3U8 parses an HLS manifest line by line and rewrites all segment/playlist/key URIs
// so that the Chromecast player requests them through the local proxy server.
func RewriteM3U8(content string, baseURL *url.URL, proxyBaseURL, origin, referer, headersJSON string) string {
	var output strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(content))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			output.WriteString("\n")
			continue
		}

		if strings.HasPrefix(line, "#") {
			// Check for tags containing URI attributes
			if strings.HasPrefix(line, "#EXT-X-KEY:") ||
				strings.HasPrefix(line, "#EXT-X-MAP:") ||
				strings.HasPrefix(line, "#EXT-X-MEDIA:") ||
				strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:") ||
				strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:") ||
				strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:") ||
				strings.HasPrefix(line, "#EXT-X-SESSION-DATA:") {

				rewrittenTag := uriAttrRegex.ReplaceAllStringFunc(line, func(match string) string {
					submatches := uriAttrRegex.FindStringSubmatch(match)
					var rawURI string
					quoted := true
					if submatches[1] != "" {
						rawURI = submatches[1]
					} else if submatches[2] != "" {
						rawURI = submatches[2]
						quoted = false
					}
					if rawURI == "" {
						return match
					}

					resolved := resolveURL(baseURL, rawURI)
					proxyURL := buildProxyLink(proxyBaseURL, resolved, origin, referer, headersJSON)
					if quoted {
						return fmt.Sprintf(`URI="%s"`, proxyURL)
					}
					return fmt.Sprintf(`URI=%s`, proxyURL)
				})

				output.WriteString(rewrittenTag)
				output.WriteString("\n")
				continue
			}

			// Plain tag without URI attributes (e.g. #EXTINF, #EXT-X-STREAM-INF, #EXT-X-TARGETDURATION)
			output.WriteString(line)
			output.WriteString("\n")
			continue
		}

		// Line does not start with '#', which means it is a segment or sub-playlist URI!
		resolved := resolveURL(baseURL, line)
		proxyURL := buildProxyLink(proxyBaseURL, resolved, origin, referer, headersJSON)
		output.WriteString(proxyURL)
		output.WriteString("\n")
	}

	return output.String()
}

func resolveURL(base *url.URL, ref string) string {
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	if base == nil {
		return ref
	}
	return base.ResolveReference(refURL).String()
}

func buildProxyLink(proxyBaseURL, targetURL, origin, referer, headersJSON string) string {
	q := url.Values{}
	q.Set("url", targetURL)
	if origin != "" {
		q.Set("origin", origin)
	}
	if referer != "" {
		q.Set("referer", referer)
	}
	if headersJSON != "" {
		q.Set("headers", headersJSON)
	}
	return fmt.Sprintf("%s/proxy?%s", proxyBaseURL, q.Encode())
}
