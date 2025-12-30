package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	defaultListenAddr        = "8080"
	defaultBaseURL           = "https://api.openai.com"
	defaultTimeout           = 60 * time.Second
	defaultNoRetryStatusCode = "400"
)

type proxyConfig struct {
	targetURL              *url.URL
	fallbackModels         []string
	defaultTimeout         time.Duration
	fallbackDefaultTimeout time.Duration
	fallbackTimeouts       map[string]time.Duration
	noRetryStatuses        map[int]struct{}
	client                 *http.Client
	baseTransport          *http.Transport
}

type cachedResponse struct {
	status int
	header http.Header
	body   []byte
}

func main() {
	gin.SetMode(gin.ReleaseMode)

	targetBase := envOrDefault("OPENAI_BASE_URL", defaultBaseURL)
	targetURL, err := url.Parse(targetBase)
	if err != nil {
		log.Fatalf("OPENAI_BASE_URL=%q is invalid: %v", targetBase, err)
	}

	cfg := newProxyConfig(targetURL)

	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())

	router.Any("/v1/chat/completions", func(c *gin.Context) {
		handleChatCompletion(c, cfg)
	})
	router.NoRoute(func(c *gin.Context) {
		c.AbortWithStatus(http.StatusNotFound)
	})

	listen := normalizeAddr(envOrDefault("PORT", defaultListenAddr))
	if err := router.Run(listen); err != nil {
		log.Fatalf("server start failed: %v", err)
	}
}

// newProxyConfig loads env settings and prepares a reusable HTTP client.
func newProxyConfig(targetURL *url.URL) *proxyConfig {
	defaultTimeoutVal := parseDurationEnv("DEFAULT_TIMEOUT", defaultTimeout)
	fallbackDefault := parseDurationEnv("FALLBACK_DEFAULT_TIMEOUT", 0)

	baseTransport := cloneDefaultTransport()
	client := &http.Client{Transport: baseTransport}

	return &proxyConfig{
		targetURL:              targetURL,
		fallbackModels:         parseModelList(envOrDefault("FALLBACK_MODELS", "")),
		defaultTimeout:         defaultTimeoutVal,
		fallbackDefaultTimeout: fallbackDefault,
		fallbackTimeouts:       parseTimeoutMap(envOrDefault("FALLBACK_TIMEOUTS", "")),
		noRetryStatuses:        parseStatusCodeSet(envOrDefault("NO_RETRY_STATUS_CODES", defaultNoRetryStatusCode)),
		client:                 client,
		baseTransport:          baseTransport,
	}
}

func handleChatCompletion(c *gin.Context, cfg *proxyConfig) {
	// Step 1: read and parse the JSON payload.
	payload, ok := readRequestPayload(c)
	if !ok {
		return
	}

	stream := isStreamRequest(payload)
	modelName := extractModelName(payload)
	if modelName == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	// Step 2: build retry model list (primary + fallbacks).
	attemptModels := buildAttemptModels(modelName, cfg.fallbackModels)
	if len(attemptModels) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "no model available"})
		return
	}

	var lastResp *cachedResponse
	var lastErr error

	// Step 3: try each model in order until success or a non-retry status.
	for idx, model := range attemptModels {
		outboundBody, err := marshalPayloadWithModel(payload, model)
		if err != nil {
			log.Printf("[proxy] marshal body failed: %v", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "invalid json body"})
			return
		}

		timeout := timeoutForAttempt(cfg, idx, model)
		resp, err := doUpstreamRequest(c.Request, cfg, outboundBody, timeout, stream)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Printf("[proxy] request canceled")
				return
			}
			lastErr = err
			log.Printf("[proxy] upstream error model=%s attempt=%d/%d: %v", model, idx+1, len(attemptModels), err)
			continue
		}

		if stream {
			if isSuccessStatus(resp.StatusCode) {
				copyHeaders(c.Writer.Header(), resp.Header)
				stripHopByHopHeaders(c.Writer.Header())
				c.Writer.WriteHeader(resp.StatusCode)
				if err := copyStream(c.Writer, resp.Body); err != nil {
					if errors.Is(err, context.Canceled) {
						log.Printf("[proxy] stream canceled")
						return
					}
					log.Printf("[proxy] stream copy failed model=%s: %v", model, err)
				}
				return
			}

			candidate, readErr := readResponse(resp)
			if readErr != nil {
				lastErr = readErr
				log.Printf("[proxy] read error response failed model=%s: %v", model, readErr)
				continue
			}

			if isNoRetryStatus(candidate.status, cfg.noRetryStatuses) {
				writeResponse(c.Writer, candidate)
				return
			}

			lastResp = candidate
			log.Printf("[proxy] retrying model=%s status=%d attempt=%d/%d", model, candidate.status, idx+1, len(attemptModels))
			continue
		}

		candidate, readErr := readResponse(resp)
		if readErr != nil {
			lastErr = readErr
			log.Printf("[proxy] read response failed model=%s: %v", model, readErr)
			continue
		}

		if isSuccessStatus(candidate.status) {
			writeResponse(c.Writer, candidate)
			return
		}

		if isNoRetryStatus(candidate.status, cfg.noRetryStatuses) {
			writeResponse(c.Writer, candidate)
			return
		}

		lastResp = candidate
		log.Printf("[proxy] retrying model=%s status=%d attempt=%d/%d", model, candidate.status, idx+1, len(attemptModels))
	}

	// Step 4: return the last upstream response if available; otherwise a 502.
	if lastResp != nil {
		writeResponse(c.Writer, lastResp)
		return
	}

	if lastErr != nil {
		log.Printf("[proxy] all attempts failed: %v", lastErr)
	}
	c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "upstream error"})
}

func readRequestPayload(c *gin.Context) (map[string]interface{}, bool) {
	if c.Request == nil || c.Request.Body == nil {
		log.Printf("[proxy] empty request body")
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "empty request body"})
		return nil, false
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("[proxy] read body failed: %v", err)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return nil, false
	}
	c.Request.Body.Close()

	if len(bodyBytes) == 0 {
		log.Printf("[proxy] empty request body")
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "empty request body"})
		return nil, false
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		log.Printf("[proxy] invalid json body: %v", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "invalid json body"})
		return nil, false
	}
	return payload, true
}

func marshalPayloadWithModel(payload map[string]interface{}, model string) ([]byte, error) {
	payload["model"] = model
	return json.Marshal(payload)
}

// doUpstreamRequest rebuilds the request body and forwards it to the target.
// Stream requests use a header timeout to avoid cutting off long responses.
func doUpstreamRequest(orig *http.Request, cfg *proxyConfig, body []byte, timeout time.Duration, stream bool) (*http.Response, error) {
	ctx := orig.Context()
	if !stream && timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	upstreamURL := buildUpstreamURL(cfg.targetURL, orig)
	req, err := http.NewRequestWithContext(ctx, orig.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header = cloneHeader(orig.Header)
	stripHopByHopHeaders(req.Header)
	req.Header.Del("Accept-Encoding")
	req.ContentLength = int64(len(body))
	req.Host = cfg.targetURL.Host
	req.Header.Set("X-Forwarded-Host", orig.Host)
	req.Header.Set("X-Forwarded-Proto", cfg.targetURL.Scheme)

	if stream && timeout > 0 {
		streamClient := newStreamClient(cfg.baseTransport, timeout)
		return streamClient.Do(req)
	}
	return cfg.client.Do(req)
}

// timeoutForAttempt chooses a timeout for primary vs fallback attempts.
func timeoutForAttempt(cfg *proxyConfig, idx int, model string) time.Duration {
	if idx == 0 {
		return cfg.defaultTimeout
	}
	if t, ok := cfg.fallbackTimeouts[model]; ok {
		return t
	}
	if cfg.fallbackDefaultTimeout > 0 {
		return cfg.fallbackDefaultTimeout
	}
	return cfg.defaultTimeout
}

func isSuccessStatus(status int) bool {
	return status >= 200 && status < 300
}

func isNoRetryStatus(status int, noRetry map[int]struct{}) bool {
	if _, ok := noRetry[status]; ok {
		return true
	}
	return false
}
