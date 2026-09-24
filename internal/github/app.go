package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// App is a GitHub App created from a manifest. Its private key is returned separately, so an App can be printed.
type App struct {
	ID       int64
	Slug     string
	ClientID string
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
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		ClientID string `json:"client_id"`
		PEM      string `json:"pem"`
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
	return App{ID: out.ID, Slug: out.Slug, ClientID: out.ClientID}, out.PEM, nil
}

// FindInstallation returns the ID of the App's installation on target, an organization or repository URL such as
// https://github.com/my-org or https://github.com/my-org/my-repo. It returns 0 if the App isn't installed there
// yet. It authenticates as the App with a JWT signed by keyPEM.
func FindInstallation(ctx context.Context, clientID, keyPEM, target string) (int64, error) {
	return findInstallation(ctx, &http.Client{Timeout: restTimeout}, defaultAPIBase, clientID, keyPEM, target)
}

func findInstallation(ctx context.Context, client *http.Client, apiBase, clientID, keyPEM, target string) (int64,
	error) {
	path, err := installationPath(target)
	if err != nil {
		return 0, err
	}
	token, err := appJWT(clientID, keyPEM, time.Now())
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	status, err := restCall(ctx, client, apiBase, http.MethodGet, path, token, &out)
	switch {
	case err != nil:
		return 0, fmt.Errorf("github: find the App's installation: %w", err)
	case status == http.StatusNotFound:
		return 0, nil
	case status != http.StatusOK:
		return 0, fmt.Errorf("github: find the App's installation: GET %s: %d %s", path, status,
			http.StatusText(status))
	}
	return out.ID, nil
}

// installationPath returns the REST API path of the App's installation on target.
func installationPath(target string) (string, error) {
	u, err := url.Parse(target)
	if err == nil && u.Host == "github.com" {
		switch parts := strings.Split(strings.Trim(u.Path, "/"), "/"); {
		case len(parts) == 1 && parts[0] != "":
			return "/orgs/" + url.PathEscape(parts[0]) + "/installation", nil
		case len(parts) == 2 && parts[0] != "" && parts[1] != "":
			return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/installation", nil
		}
	}
	return "", fmt.Errorf("github: %q must be https://github.com/<org> or https://github.com/<owner>/<repo>", target)
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
