// Package gcm provides send and receive GCM functionality.
package gcm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	httpAddress = "https://gcm-http.googleapis.com/gcm/send"
)

// httpClient is an interface to stub the internal http.Client.
type httpClient interface {
	Do(req *http.Request) (resp *http.Response, err error)
}

// gcmHTTP is a container for the GCM HTTP Server client.
type gcmHTTP struct {
	GCMURL     string
	apiKey     string
	httpClient httpClient
	debug      bool
}

// Used to compute results for multicast messages with retries.
type multicastResultsState map[string]*HTTPResult

// newHTTPGCMClient creates a new client for handling GCM HTTP requests.
func newHTTPClient(apiKey string, debug bool) httpC {
	return &gcmHTTP{
		GCMURL:     httpAddress,
		apiKey:     apiKey,
		httpClient: &http.Client{},
		debug:      debug,
	}
}

// Send sends an HTTP message using exponential backoff, handling multicast replies.
func (c *gcmHTTP) Send(m HTTPMessage) (*HTTPResponse, error) {
	targets, err := messageTargetAsStringsArray(m)
	if err != nil {
		return nil, fmt.Errorf("error extracting target from message: %v", err)
	}

	var (
		multicastID  int64
		retryAfter   time.Duration
		gcmResp      *HTTPResponse
		b            = newExponentialBackoff()
		resultsState = make(multicastResultsState)
		localTo      = make([]string, len(targets))
	)

	// Make a copy of the targets to keep track of results during retries.
	copy(localTo, targets)

	for b.sendAnother() {
		if gcmResp, retryAfter, err = sendHTTP(c.httpClient, c.GCMURL, c.apiKey, m, c.debug); err != nil {
			// Honor the Retry-After header if it is included in the response.
			if retryAfter > 0 {
				b.setMin(retryAfter)
				b.wait()
				continue
			}
			return nil, err
		}
		if len(gcmResp.Results) > 0 {
			multicastID = gcmResp.MulticastID
			doRetry, toRetry, err := checkResults(gcmResp.Results, localTo, resultsState)
			if err != nil {
				return gcmResp, fmt.Errorf("error checking GCM results: %v", err)
			}
			if doRetry {
				// Honor the Retry-After header if it is included in the response.
				if retryAfter > 0 {
					b.setMin(retryAfter)
				}
				localTo = make([]string, len(toRetry))
				copy(localTo, toRetry)
				if m.RegistrationIDs != nil {
					m.RegistrationIDs = toRetry
				}
				b.wait()
				continue
			}
		}
		break
	}

	// if it was multicast, reconstruct response in case there have been retries
	if len(targets) > 1 {
		gcmResp = buildRespForMulticast(targets, resultsState, multicastID)
	}

	return gcmResp, nil
}

// parseRetryAfter returns the duration to wait until the next retry attempt.
func parseRetryAfter(retryAfter string) (time.Duration, error) {
	// W3 spec, section 14.37:
	// Retry-After  = "Retry-After" ":" ( HTTP-date | delta-seconds )
	// Examples:
	// Retry-After: Fri, 31 Dec 1999 23:59:59 GMT
	// Retry-After: 120
	// Assuming that the header contains seconds instead of a date

	// Try parsing seconds first:
	if d, err := time.ParseDuration(fmt.Sprintf("%vs", retryAfter)); err == nil {
		return d, nil
	}

	// Try parsing http date
	if t, err := http.ParseTime(retryAfter); err == nil {
		d := t.Sub(time.Now().UTC())
		if d < 0 {
			d = 0
		}
		return d, nil
	}

	return 0, fmt.Errorf("cannot parse Retry-After header %s", retryAfter)
}

// sendHTTP sends a single request to GCM HTTP server and parses the response.
func sendHTTP(httpClient httpClient, URL string, apiKey string, m HTTPMessage,
	debug bool) (gcmResp *HTTPResponse, retryAfter time.Duration, err error) {
	var bs []byte
	if bs, err = json.Marshal(m); err != nil {
		return
	}

	if debug {
		log.WithField("http request", string(bs)).Debug("gcm http request")
	}

	var req *http.Request
	if req, err = http.NewRequest("POST", URL, bytes.NewReader(bs)); err != nil {
		return
	}

	// Add required headers.
	req.Header.Add(http.CanonicalHeaderKey("Content-Type"), "application/json")
	req.Header.Add(http.CanonicalHeaderKey("Authorization"), fmt.Sprintf("key=%v", apiKey))

	var httpResp *http.Response
	if httpResp, err = httpClient.Do(req); err != nil {
		return
	}

	gcmResp = &HTTPResponse{StatusCode: httpResp.StatusCode}
	retryAfter, err = parseRetryAfter(httpResp.Header.Get(http.CanonicalHeaderKey("Retry-After")))

	// Read response. Valid response body is guaranteed to exist only with response status 200.
	var body []byte
	if body, err = ioutil.ReadAll(httpResp.Body); err != nil && httpResp.StatusCode == http.StatusOK {
		err = fmt.Errorf("error reading http response body: %v", err)
		return
	}
	defer httpResp.Body.Close()

	// Parse response if appicable.
	if len(body) > 0 {
		if debug {
			log.WithFields(log.Fields{
				"status": httpResp.StatusCode,
				"body":   string(body),
			}).Debug("gcm http reply")
		}
		err = json.Unmarshal(body, gcmResp)
	}

	return
}

// buildRespForMulticast builds the final response for a multicast message, in case there have been
// retries for subsets of the original recipients.
func buildRespForMulticast(to []string, mrs multicastResultsState, mid int64) *HTTPResponse {
	resp := &HTTPResponse{}
	resp.MulticastID = mid
	resp.Results = make([]HTTPResult, len(to))
	for i, regID := range to {
		result, ok := mrs[regID]
		if !ok {
			continue
		}
		resp.Results[i] = *result
		if result.MessageID != "" {
			resp.Success++
		} else if result.Error != "" {
			resp.Failure++
		}
		if result.RegistrationID != "" {
			resp.CanonicalIds++
		}
	}
	return resp
}

// messageTargetAsStringsArray transforms the recipient in an array of strings if needed.
func messageTargetAsStringsArray(m HTTPMessage) ([]string, error) {
	if m.RegistrationIDs != nil {
		return m.RegistrationIDs, nil
	} else if m.To != "" {
		target := []string{m.To}
		return target, nil
	}
	target := []string{}
	return target, fmt.Errorf("cannot find any valid target field in message")
}

// checkResults determines which errors can be retried in the multicast send.
func checkResults(gcmResults []HTTPResult, recipients []string,
	resultsState multicastResultsState) (doRetry bool, toRetry []string, err error) {
	doRetry = false
	toRetry = []string{}
	for i := 0; i < len(gcmResults); i++ {
		result := gcmResults[i]
		regID := recipients[i]
		resultsState[regID] = &result
		if result.Error != "" {
			if retryableErrors[result.Error] {
				toRetry = append(toRetry, regID)
				if doRetry == false {
					doRetry = true
				}
			}
		}
	}
	return doRetry, toRetry, nil
}
