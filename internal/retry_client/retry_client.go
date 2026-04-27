package retry_client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
)

const (
	githubSecondaryRateLimitMessage = `You have exceeded a secondary rate limit`
	retryableContextKey             = "gitea-github-migrator"
)

var secondaryRateLimitDocumentationSuffixes = []string{
	`secondary-rate-limits`,
	`#abuse-rate-limits`,
}

type gitHubError struct {
	Message          string
	DocumentationURL string `json:"documentation_url"`
}

func New(logger hclog.Logger) *retryablehttp.Client {
	retryClient := &retryablehttp.Client{
		HTTPClient:   cleanhttp.DefaultPooledClient(),
		Logger:       nil,
		RetryMax:     8,
		RetryWaitMin: 30 * time.Second,
		RetryWaitMax: 300 * time.Second,
	}

	retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) (sleep time.Duration) {
		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		defer func() {
			logger.Warn("waiting before retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "sleep", sleep, "attempt", attemptNum, "max_attempts", retryClient.RetryMax)
		}()

		if resp != nil {

			var isSecondaryRateLimit bool
			var errResp gitHubError
			if resp.Request != nil {
				if v := resp.Request.Context().Value(retryableContextKey); v != nil {
					errResp = v.(gitHubError)
					isSecondaryRateLimit = strings.HasPrefix(errResp.Message, githubSecondaryRateLimitMessage) ||
						slices.ContainsFunc(secondaryRateLimitDocumentationSuffixes, func(suffix string) bool {
							return strings.HasSuffix(errResp.DocumentationURL, suffix)
						})
				}
			}

			// Check the Retry-After header
			if s, ok := resp.Header["Retry-After"]; ok {
				if retryAfter, err := strconv.ParseInt(s[0], 10, 64); err == nil {
					sleep = time.Second * time.Duration(retryAfter)
					return
				}
			}

			// Reference:
			// - https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api?apiVersion=2022-11-28
			// - https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2022-11-28
			if v, ok := resp.Header["X-Ratelimit-Remaining"]; ok {
				if remaining, err := strconv.ParseInt(v[0], 10, 64); err == nil && (remaining == 0 || isSecondaryRateLimit) {

					// If x-ratelimit-reset is present, this indicates the UTC timestamp when we can retry
					if w, ok := resp.Header["X-Ratelimit-Reset"]; ok {
						if recoveryEpoch, err := strconv.ParseInt(w[0], 10, 64); err == nil {
							// Add 30 seconds to recovery timestamp for clock differences
							sleep = roundDuration(time.Until(time.Unix(recoveryEpoch+30, 0)), time.Second)
							return
						}
					}

					// Otherwise, wait for 60 seconds
					sleep = 60 * time.Second
					return
				}
			}
		}

		// Exponential backoff
		mult := math.Pow(2, float64(attemptNum)) * float64(min)
		wait := time.Duration(mult)
		if float64(wait) != mult || wait > max {
			wait = max
		}

		sleep = wait
		return
	}

	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return false, err
		}

		// Potential connection reset
		if resp == nil {
			return true, nil
		}

		errResp := gitHubError{}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			if err = unmarshalResp(resp, &errResp); err != nil {
				return false, err
			}
		}

		// Token not authorized for org
		if resp.StatusCode == http.StatusForbidden {
			if match, err := regexp.MatchString("SAML enforcement", errResp.Message); err != nil {
				return false, fmt.Errorf("matching 403 response: %v", err)
			} else if match {
				msg := errResp.Message
				if errResp.DocumentationURL != "" {
					msg += fmt.Sprintf(" - %s", errResp.DocumentationURL)
				}
				return false, fmt.Errorf("received 403 with response: %v", msg)
			}
		}

		retryableStatuses := []int{
			http.StatusTooManyRequests, // rate-limiting
			http.StatusForbidden,       // rate-limiting

			http.StatusRequestTimeout,
			http.StatusFailedDependency,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		}

		requestMethod := "unknown"
		requestUrl := "unknown"

		if req := resp.Request; req != nil {
			requestMethod = req.Method
			if req.URL != nil {
				requestUrl = req.URL.String()
			}
		}

		for _, status := range retryableStatuses {
			if resp.StatusCode == status {

				// See https://github.com/hashicorp/go-retryablehttp/issues/215#issuecomment-4175283369 why we override the request to inject the errResp object into its context
				if req := resp.Request; req != nil {
					*req = *req.WithContext(context.WithValue(ctx, retryableContextKey, errResp))
				}

				logger.Warn("retrying failed API request", "method", requestMethod, "url", requestUrl, "status", resp.StatusCode, "message", errResp.Message)
				return true, nil
			}
		}

		return false, nil
	}

	return retryClient
}

func roundDuration(d, r time.Duration) time.Duration {
	if r <= 0 {
		return d
	}
	neg := d < 0
	if neg {
		d = -d
	}
	if m := d % r; m+m < r {
		d = d - m
	} else {
		d = d + r - m
	}
	if neg {
		return -d
	}
	return d
}

func unmarshalResp(resp *http.Response, model interface{}) error {
	if resp == nil {
		return nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("parsing response body: %+v", err)
	}
	defer func(body io.ReadCloser) {
		_ = body.Close()
	}(resp.Body)

	// Trim away a BOM if present
	respBody = bytes.TrimPrefix(respBody, []byte("\xef\xbb\xbf"))

	// In some cases the respBody is empty, but not nil, so don't attempt to unmarshal this
	if len(respBody) == 0 {
		return nil
	}

	// Unmarshal into provided model
	if err := json.Unmarshal(respBody, model); err != nil {
		return fmt.Errorf("unmarshaling response body: %+v", err)
	}

	// Reassign the response body as downstream code may expect it
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	return nil
}
