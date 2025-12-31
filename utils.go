package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// readResponse buffers the upstream response so we can reuse it on retry or final response.
func readResponse(resp *http.Response) (*cachedResponse, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &cachedResponse{
		status: resp.StatusCode,
		header: cloneHeader(resp.Header),
		body:   body,
	}, nil
}

func writeResponse(w http.ResponseWriter, resp *cachedResponse) {
	copyHeaders(w.Header(), resp.header)
	stripHopByHopHeaders(w.Header())
	w.Header().Del("Content-Length")
	if len(resp.body) > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(resp.body)))
	}
	w.WriteHeader(resp.status)
	if len(resp.body) > 0 {
		_, _ = w.Write(resp.body)
	}
}

func copyStream(dst http.ResponseWriter, src io.Reader) error {
	buf := make([]byte, 32*1024)
	flusher, _ := dst.(http.Flusher)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	copyHeaders(dst, src)
	return dst
}

func stripHopByHopHeaders(h http.Header) {
	for _, key := range hopByHopHeaders {
		h.Del(key)
	}
}

func newStreamClient(base *http.Transport, timeout time.Duration) *http.Client {
	transport := base.Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.DisableKeepAlives = true
	return &http.Client{Transport: transport}
}

func buildUpstreamURL(base *url.URL, orig *http.Request) *url.URL {
	target := *base
	target.Path = singleJoiningSlash(base.Path, orig.URL.Path)
	target.RawQuery = joinQuery(base.RawQuery, orig.URL.RawQuery)
	return &target
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func joinQuery(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "&" + b
}

func isStreamRequest(payload map[string]interface{}) bool {
	raw, ok := payload["stream"]
	if !ok {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		l := strings.ToLower(strings.TrimSpace(v))
		return l == "true" || l == "1" || l == "yes" || l == "on"
	case float64:
		return v != 0
	default:
		return false
	}
}

func extractModelName(payload map[string]interface{}) string {
	raw, ok := payload["model"]
	if !ok {
		return ""
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func buildAttemptModels(primary string, fallbacks []string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		if _, ok := seen[model]; ok {
			return
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	add(primary)
	for _, model := range fallbacks {
		add(model)
	}
	return out
}

func parseModelList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		model := strings.TrimSpace(part)
		if model == "" {
			continue
		}
		out = append(out, model)
	}
	return out
}

func parseTimeoutMap(raw string) map[string]time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[string]time.Duration)
	for _, part := range strings.Split(raw, ",") {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		pair := strings.SplitN(item, "=", 2)
		if len(pair) != 2 {
			log.Printf("[config] invalid timeout pair: %q", item)
			continue
		}
		model := strings.TrimSpace(pair[0])
		value := strings.TrimSpace(pair[1])
		if model == "" || value == "" {
			log.Printf("[config] invalid timeout pair: %q", item)
			continue
		}
		if dur, ok := parseDurationString(value); ok {
			out[model] = dur
			continue
		}
		log.Printf("[config] invalid timeout value for model=%s: %q", model, value)
	}
	return out
}

func parseStatusCodeSet(raw string) map[int]struct{} {
	out := make(map[int]struct{})
	for _, part := range strings.Split(raw, ",") {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		code, err := strconv.Atoi(item)
		if err != nil {
			log.Printf("[config] invalid status code: %q", item)
			continue
		}
		out[code] = struct{}{}
	}
	return out
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	if dur, ok := parseDurationString(raw); ok {
		return dur
	}
	log.Printf("[config] invalid duration %s=%q, using fallback %s", key, raw, fallback)
	return fallback
}

func parseDurationString(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if dur, err := time.ParseDuration(raw); err == nil {
		return dur, true
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}

func envOrDefault(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

func normalizeAddr(port string) string {
	if strings.HasPrefix(port, ":") {
		return port
	}
	return ":" + port
}

func cloneDefaultTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	return base.Clone()
}

func closeBody(body io.Closer, hint string) {
	if body == nil {
		return
	}
	if err := body.Close(); err != nil {
		log.Printf("[proxy] close %s failed: %v", hint, err)
	}
}
