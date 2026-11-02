// Package kvaultclient talks to a KVault server's Service-Token-
// authenticated surfaces — the same auth model as the "CI token" paths
// documented in KVault's own CLAUDE.md ("CI self-enrollment" / "Fleet
// rebuilds (rekey)" sections): a Cloudflare Access Service Token presented
// via CF-Access-Client-Id/-Secret headers, not a human SSO session and not
// a per-machine signed request (that's the separate machine-key auth path,
// which this provider never uses — the provider authenticates as the CI
// token itself, the same credential a Terraform apply would otherwise
// bake into a self-enroll call).
package kvaultclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	BaseURL        string
	CIClientID     string
	CIClientSecret string
	HTTPClient     *http.Client
}

func New(baseURL, ciClientID, ciClientSecret string) *Client {
	return &Client{
		BaseURL:        baseURL,
		CIClientID:     ciClientID,
		CIClientSecret: ciClientSecret,
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError carries the HTTP status code alongside the message, so callers
// can distinguish e.g. "not found" (404) from other failures without
// parsing the error string.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is an *APIError with Status 404 — the
// expected, non-error outcome when resolving a machine name that hasn't
// enrolled yet (see ResolveMachineKey).
func IsNotFound(err error) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.Status == http.StatusNotFound
}

type apiErrorBody struct {
	Error string `json:"error"`
}

func apiErrorFrom(status int, body []byte) error {
	var e apiErrorBody
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return &APIError{Status: status, Message: e.Error}
	}
	return &APIError{Status: status, Message: string(body)}
}

func (c *Client) do(method, path string, body any, wantStatus int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	reqURL := c.BaseURL + path
	req, err := http.NewRequest(method, reqURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("CF-Access-Client-Id", c.CIClientID)
	req.Header.Set("CF-Access-Client-Secret", c.CIClientSecret)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s %s: %w", method, reqURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s %s: %w", method, reqURL, err)
	}
	if resp.StatusCode != wantStatus {
		return nil, apiErrorFrom(resp.StatusCode, respBody)
	}
	return respBody, nil
}

type EnrollCodeResult struct {
	Code             string `json:"code"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
	Mode             string `json:"mode"` // "create" or "rekey"
	KeyID            string `json:"keyId,omitempty"`
}

// EnrollCode mints a short-lived, single-use, name-bound enrollment code
// via POST /api/machine-keys/enroll-code. If keyID is non-empty, the
// server mints a rekey-mode code for that exact identity (and 403s if this
// token didn't enroll it); if empty, the server resolves identity by name
// itself (0 matches -> create mode, 1 -> rekey, 2+ -> 409).
func (c *Client) EnrollCode(name, keyID string, expiresInSeconds int, bindSourceIP string) (*EnrollCodeResult, error) {
	payload := map[string]any{"name": name}
	if keyID != "" {
		payload["keyId"] = keyID
	}
	if expiresInSeconds > 0 {
		payload["expiresInSeconds"] = expiresInSeconds
	}
	if bindSourceIP != "" {
		payload["bindSourceIp"] = bindSourceIP
	}

	respBody, err := c.do(http.MethodPost, "/api/machine-keys/enroll-code", payload, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	var result EnrollCodeResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing enroll-code response: %w", err)
	}
	return &result, nil
}

// ResolveMachineKey looks up the keyId a machine name has resolved to via
// GET /api/machine-keys/resolve?name=<name> — populated only once a host
// has actually redeemed its enrollment code. Returns an *APIError with
// Status 404 (check with IsNotFound) if no machine by that name has
// enrolled yet under this token — that is an expected, non-fatal outcome
// for a resource whose host hasn't booted yet, not necessarily a real
// error; callers decide how to treat it.
func (c *Client) ResolveMachineKey(name string) (string, error) {
	respBody, err := c.do(http.MethodGet, "/api/machine-keys/resolve?name="+url.QueryEscape(name), nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var result struct {
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing resolve response: %w", err)
	}
	return result.KeyID, nil
}

// RevokeMachineKey deletes a machine's enrollment via
// DELETE /api/machine-keys/:id — only permitted for a canMintEnrollments
// token against a machine IT enrolled; the server rejects anything else.
func (c *Client) RevokeMachineKey(keyID string) error {
	_, err := c.do(http.MethodDelete, "/api/machine-keys/"+url.PathEscape(keyID), nil, http.StatusOK)
	return err
}

// FetchSecret retrieves a secret's value via the ordinary Service-Token
// auth path (GET /api/secret/:key) — the same route a Kubernetes/ESO
// caller uses, authenticated as the provider's own CI Service Token
// rather than a machine key. Used by the ephemeral secret data source;
// the returned value must never be written to Terraform state.
func (c *Client) FetchSecret(key, namespace string) (string, error) {
	path := "/api/secret/" + url.PathEscape(key)
	if namespace != "" {
		path += "?namespace=" + url.QueryEscape(namespace)
	}
	respBody, err := c.do(http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing fetch response: %w", err)
	}
	return result.Value, nil
}
