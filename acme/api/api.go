package api

import (
	"bytes"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-acme/lego/v4/acme"
	"github.com/go-acme/lego/v4/acme/api/internal/nonces"
	"github.com/go-acme/lego/v4/acme/api/internal/secure"
	"github.com/go-acme/lego/v4/acme/api/internal/sender"
	"github.com/go-acme/lego/v4/log"
)

// Core ACME/LE core API.
type Core struct {
	doer         *sender.Doer
	nonceManager *nonces.Manager
	jws          *secure.JWS
	directory    acme.Directory
	HTTPClient   *http.Client

	common         service // Reuse a single struct instead of allocating one for each service on the heap.
	Accounts       *AccountService
	Authorizations *AuthorizationService
	Certificates   *CertificateService
	Challenges     *ChallengeService
	Orders         *OrderService
}

// New Creates a new Core.
func New(httpClient *http.Client, userAgent, caDirURL, kid string, privateKey crypto.PrivateKey) (*Core, error) {
	doer := sender.NewDoer(httpClient, userAgent)

	dir, err := getDirectory(doer, caDirURL)
	if err != nil {
		return nil, err
	}

	nonceManager := nonces.NewManager(doer, dir.NewNonceURL)

	jws := secure.NewJWS(privateKey, kid, nonceManager)

	c := &Core{doer: doer, nonceManager: nonceManager, jws: jws, directory: dir, HTTPClient: httpClient}

	c.common.core = c
	c.Accounts = (*AccountService)(&c.common)
	c.Authorizations = (*AuthorizationService)(&c.common)
	c.Certificates = (*CertificateService)(&c.common)
	c.Challenges = (*ChallengeService)(&c.common)
	c.Orders = (*OrderService)(&c.common)

	return c, nil
}

// post performs an HTTP POST request and parses the response body as JSON,
// into the provided respBody object.
func (a *Core) post(uri string, reqBody, response any) (*http.Response, error) {
	content, err := json.Marshal(reqBody)
	if err != nil {
		return nil, errors.New("failed to marshal message")
	}

	return a.retrievablePost(uri, content, response)
}

// postAsGet performs an HTTP POST ("POST-as-GET") request.
// https://www.rfc-editor.org/rfc/rfc8555.html#section-6.3
func (a *Core) postAsGet(uri string, response any) (*http.Response, error) {
	return a.retrievablePost(uri, []byte{}, response)
}

func (a *Core) retrievablePost(uri string, content []byte, response any) (*http.Response, error) {
	// during tests, allow to support ~90% of bad nonce with a minimum of attempts.
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 200 * time.Millisecond
	bo.MaxInterval = 5 * time.Second
	bo.MaxElapsedTime = 20 * time.Second

	var resp *http.Response
	operation := func() error {
		var err error
		resp, err = a.signedPost(uri, content, response)
		if err != nil {
			// Retry if the nonce was invalidated
			var e *acme.NonceError
			if errors.As(err, &e) {
				return err
			}

			return backoff.Permanent(err)
		}

		return nil
	}

	notify := func(err error, duration time.Duration) {
		log.Infof("retry due to: %v", err)
	}

	err := backoff.RetryNotify(operation, bo, notify)
	if err != nil {
		return resp, err
	}

	return resp, nil
}

func (a *Core) signedPost(uri string, content []byte, response any) (*http.Response, error) {
	signedContent, err := a.jws.SignContent(uri, content)
	if err != nil {
		return nil, fmt.Errorf("failed to post JWS message: failed to sign content: %w", err)
	}

	signedBody := bytes.NewBufferString(signedContent.FullSerialize())

	resp, err := a.doer.Post(uri, signedBody, "application/jose+json", response)

	// nonceErr is ignored to keep the root error.
	nonce, nonceErr := nonces.GetFromResponse(resp)
	if nonceErr == nil {
		a.nonceManager.Push(nonce)
	}

	return resp, err
}

func (a *Core) signEABContent(newAccountURL, kid string, hmac []byte) ([]byte, error) {
	eabJWS, err := a.jws.SignEABContent(newAccountURL, kid, hmac)
	if err != nil {
		return nil, err
	}

	return []byte(eabJWS.FullSerialize()), nil
}

// GetKeyAuthorization Gets the key authorization.
func (a *Core) GetKeyAuthorization(token string) (string, error) {
	return a.jws.GetKeyAuthorization(token)
}

func (a *Core) GetDirectory() acme.Directory {
	return a.directory
}

func getDirectory(do *sender.Doer, caDirURL string) (acme.Directory, error) {
	var dir acme.Directory
	if _, err := do.Get(caDirURL, &dir); err != nil {
		return dir, fmt.Errorf("get directory at '%s': %w", caDirURL, err)
	}

	if dir.NewAccountURL == "" {
		return dir, errors.New("directory missing new registration URL")
	}
	if dir.NewOrderURL == "" {
		return dir, errors.New("directory missing new order URL")
	}

	return dir, nil
}
