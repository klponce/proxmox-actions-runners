package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// App is a GitHub App created from a manifest. Its private key is returned separately, so an App can be printed.
type App struct {
	ID       int64
	Slug     string
	ClientID string
	// Owner is the account the App belongs to, and OwnerType is "Organization" or "User". A private App can only be
	// installed on its owner.
	Owner     string
	OwnerType string
}

// ConvertManifest exchanges the code GitHub hands back after the user creates an App from a manifest for the App
// and its PEM private key. The code is single-use and expires after an hour. The request needs no authentication:
// the code is the credential, so neither it nor the key is ever logged or put in an error. The App's client secret
// and webhook secret are dropped, because the controller doesn't use them.
func ConvertManifest(ctx context.Context, code string) (App, string, error) {
	return convertManifest(ctx, &http.Client{Timeout: restTimeout}, defaultAPIBase, code)
}

func convertManifest(ctx context.Context, client *http.Client, apiBase, code string) (App, string, error) {
	var out struct {
		ID       int64   `json:"id"`
		Slug     string  `json:"slug"`
		ClientID string  `json:"client_id"`
		PEM      string  `json:"pem"`
		Owner    account `json:"owner"`
	}
	status, err := restCall(ctx, client, apiBase, http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions",
		"", &out)
	switch {
	case err != nil:
		return App{}, "", fmt.Errorf("github: convert the App manifest code: %w", err)
	case status == http.StatusNotFound || status == http.StatusUnprocessableEntity:
		return App{}, "", fmt.Errorf("github: convert the App manifest code: %d %s: the code is invalid, already "+
			"used, or more than an hour old", status, http.StatusText(status))
	case status != http.StatusCreated:
		return App{}, "", fmt.Errorf("github: convert the App manifest code: %d %s", status, http.StatusText(status))
	}
	if err := CheckPrivateKey(out.PEM); err != nil {
		return App{}, "", err
	}
	return App{ID: out.ID, Slug: out.Slug, ClientID: out.ClientID, Owner: out.Owner.Login,
		OwnerType: out.Owner.Type}, out.PEM, nil
}

type account struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Installation is where a GitHub App is installed.
type Installation struct {
	ID int64
	// Account is the organization or personal account, and AccountType is "Organization" or "User".
	Account     string
	AccountType string
	// Repositories are the names of the repositories an installation on a personal account can reach. GitHub allows
	// self-hosted runners on a personal account only per repository, so the installer picks one of them. They aren't
	// listed for an organization, whose runners serve the whole organization.
	Repositories []string
}

// FindInstallation returns the App's installation, or a zero Installation if the App isn't installed yet. A private
// App can only be installed on the account that owns it, so it has at most one. It authenticates as the App with a
// JWT signed by keyPEM.
func FindInstallation(ctx context.Context, clientID, keyPEM string) (Installation, error) {
	return findInstallation(ctx, &http.Client{Timeout: restTimeout}, defaultAPIBase, clientID, keyPEM)
}

func findInstallation(ctx context.Context, client *http.Client, apiBase, clientID, keyPEM string) (Installation,
	error) {
	jwt, err := appJWT(clientID, keyPEM, time.Now())
	if err != nil {
		return Installation{}, err
	}
	var installations []struct {
		ID      int64   `json:"id"`
		Account account `json:"account"`
	}
	if err := restGet(ctx, client, apiBase, "/app/installations", jwt, &installations); err != nil {
		return Installation{}, fmt.Errorf("github: find the App's installation: %w", err)
	}
	if len(installations) == 0 {
		return Installation{}, nil
	}
	in := installations[0]
	found := Installation{ID: in.ID, Account: in.Account.Login, AccountType: in.Account.Type}
	if found.AccountType != "User" {
		return found, nil
	}

	var token struct {
		Token string `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", in.ID)
	status, err := restCall(ctx, client, apiBase, http.MethodPost, path, jwt, &token)
	switch {
	case err != nil:
		return Installation{}, fmt.Errorf("github: list the installation's repositories: %w", err)
	case status != http.StatusCreated:
		return Installation{}, fmt.Errorf("github: list the installation's repositories: POST %s: %d %s", path, status,
			http.StatusText(status))
	}
	var repos struct {
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	}
	if err := restGet(ctx, client, apiBase, "/installation/repositories?per_page=100", token.Token,
		&repos); err != nil {
		return Installation{}, fmt.Errorf("github: list the installation's repositories: %w", err)
	}
	for _, r := range repos.Repositories {
		found.Repositories = append(found.Repositories, r.Name)
	}
	return found, nil
}

// restGet is a GET that must answer 200.
func restGet(ctx context.Context, client *http.Client, apiBase, path, token string, out any) error {
	status, err := restCall(ctx, client, apiBase, http.MethodGet, path, token, out)
	switch {
	case err != nil:
		return err
	case status != http.StatusOK:
		return fmt.Errorf("GET %s: %d %s", path, status, http.StatusText(status))
	}
	return nil
}

// appJWT returns a JWT that authenticates as the App for a few minutes. It is backdated a minute to allow for clock
// drift, and GitHub rejects one that expires more than ten minutes after it was issued.
func appJWT(clientID, keyPEM string, now time.Time) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(keyPEM))
	if err != nil {
		return "", errors.New("github: App private key is not a valid RSA key")
	}
	claims := jwt.RegisteredClaims{
		Issuer:    clientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}
