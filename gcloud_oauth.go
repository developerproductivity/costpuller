package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// defaultTokenCachePath is the path, relative to the platform's user cache
// directory, to the directory where cache files are stored.
const defaultTokenCachePath = "gcloud"

// tokenFileName is the name of the file which is used to store the cached
// OAuth 2.0 access and refresh token values.
const tokenFileName = "costpuller_token.json"

// costpullerGoogleCredentialsEnv is the environment variable that points to the
// Google credentials JSON file for this program only. When set, Application Default
// Credentials do not use GOOGLE_APPLICATION_CREDENTIALS, so other tools on the
// same machine can keep using that variable without affecting costpuller.
const costpullerGoogleCredentialsEnv = "COSTPULLER_GOOGLE_CREDENTIALS"

const googleSheetsScope = "https://www.googleapis.com/auth/spreadsheets"

// oauthDebugf logs only when costpullerDebug is true (-debug, set in main after flag.Parse).
func oauthDebugf(format string, args ...any) {
	if costpullerDebug {
		log.Printf(format, args...)
	}
}

// oauthDebugGoogleClientReady logs one line after credentials JSON is loaded and
// oauth2.Config is built. For more detail later, consider: JSON log fields (e.g.
// encoding/json on a struct), Go log/slog with attributes, or OpenTelemetry spans
// instead of growing printf-style lines.
func oauthDebugGoogleClientReady(credObj *google.Credentials, jsonType string, jsonTypeErr error, clientID string) {
	if !costpullerDebug {
		return
	}
	t := jsonType
	if jsonTypeErr != nil {
		t = fmt.Sprintf("<?> (%v)", jsonTypeErr)
	}
	var src string
	if p := strings.TrimSpace(os.Getenv(costpullerGoogleCredentialsEnv)); p != "" {
		src = fmt.Sprintf("%s=%q", costpullerGoogleCredentialsEnv, p)
	} else {
		src = fmt.Sprintf("ADC(%s unset)", costpullerGoogleCredentialsEnv)
	}
	log.Printf("[getGoogleOAuthHttpClient] %s project_id=%q json_bytes=%d json_type=%s client_id=%q",
		src, credObj.ProjectID, len(credObj.JSON), t, clientID)
}

// loadGoogleCredentials loads Google OAuth or service-account JSON for the
// Sheets API scope (googleSheetsScope).
//
// If the environment variable named by costpullerGoogleCredentialsEnv is set to a
// file path, that file is read and GOOGLE_APPLICATION_CREDENTIALS is not used
// for this load. Otherwise Application Default Credentials apply (including
// GOOGLE_APPLICATION_CREDENTIALS and well-known paths such as
// ~/.config/gcloud/application_default_credentials.json).
func loadGoogleCredentials(ctx context.Context) (*google.Credentials, error) {
	if p := strings.TrimSpace(os.Getenv(costpullerGoogleCredentialsEnv)); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s %q: %w", costpullerGoogleCredentialsEnv, p, err)
		}
		return google.CredentialsFromJSON(ctx, b, googleSheetsScope)
	}
	return google.FindDefaultCredentials(ctx, googleSheetsScope)
}

// googleCredentialsSourceSummary describes which non-secret credential source
// loadGoogleCredentials used (path or ADC). Never includes file contents or tokens.
func googleCredentialsSourceSummary() string {
	if p := strings.TrimSpace(os.Getenv(costpullerGoogleCredentialsEnv)); p != "" {
		return fmt.Sprintf("%s file %q", costpullerGoogleCredentialsEnv, p)
	}
	return fmt.Sprintf("Application Default Credentials (%s unset)", costpullerGoogleCredentialsEnv)
}

// getGoogleOAuthHttpClient accepts a mapping of configuration value strings
// and returns an HTTP client which can be used to make authorized Google API
// requests.  The token is obtained either using values cached in a local file
// or by prompting the user to perform an authorization dialog; either way, the
// new token is written to the cache file before returning.
//
// Client JSON is resolved by loadGoogleCredentials (see costpullerGoogleCredentialsEnv).
// Credentials can be created in Google Cloud Console under "Credentials".
func getGoogleOAuthHttpClient(oauthConfigMap Configuration) *http.Client {
	ctx := context.Background()

	credObj, err := loadGoogleCredentials(ctx)
	if err != nil {
		log.Fatalf("[getGoogleOAuthHttpClient] Unable to read OAuth client credentials file: %v", err)
	}

	var credType struct {
		Type string `json:"type"`
	}
	typeErr := json.Unmarshal(credObj.JSON, &credType)

	config, err := google.ConfigFromJSON(credObj.JSON, googleSheetsScope)
	if err != nil {
		src := googleCredentialsSourceSummary()
		log.Fatalf(
			"[getGoogleOAuthHttpClient] Unable to construct oauth2.Config (source: %s; credential JSON type %q; json_bytes=%d): %v",
			src, credType.Type, len(credObj.JSON), err,
		)
	}

	oauthDebugGoogleClientReady(credObj, credType.Type, typeErr, config.ClientID)

	token, tokenCachePath := getToken(oauthConfigMap, config, ctx)
	cacheToken(token, tokenCachePath)

	return config.Client(ctx, token)
}

// getToken is a helper function which extracts configuration information from
// the supplied mapping and returns either a cached token, if available, or a
// new token. When the cache path cannot be resolved, the cache file is missing,
// or the cached token cannot be refreshed, execution falls through to a single
// getNewToken path at the end (same as the original control flow).
func getToken(
	oauthConfigMap Configuration,
	config *oauth2.Config,
	ctx context.Context,
) (token *oauth2.Token, tokenCachePath string) {
	path := getMapKeyString(oauthConfigMap, "tokenCachePath", "")
	tokenCachePath, err := getCacheFileName(path)
	if err != nil {
		oauthDebugf("[getToken] Unable to determine cache path; new token will not be cached on disk")
	} else {
		tokenCacheFile, err := os.Open(tokenCachePath)
		if err == nil {
			token, err = getCachedTokenSafe(config, tokenCacheFile, ctx)
			closeFile(tokenCacheFile)
			if err == nil {
				oauthDebugf("[getToken] Using cached token from %q", tokenCachePath)
				return token, tokenCachePath
			}
			oauthDebugf("[getToken] Cached token invalid, will get a new token")
			if removeErr := os.Remove(tokenCachePath); removeErr != nil {
				log.Printf("[getToken] Warning: unable to delete invalid cached token: %v", removeErr)
			}
		} else {
			if errors.Is(err, os.ErrNotExist) {
				oauthDebugf("[getToken] No token cache file at %q", tokenCachePath)
			} else {
				oauthDebugf("[getToken] Cannot open token cache %q: %v; using browser auth", tokenCachePath, err)
			}
		}
	}

	oauthDebugf("[getToken] Getting new OAuth token")
	port := getMapKeyString(oauthConfigMap, "port", "")
	token = getNewToken(config, port, ctx)
	return token, tokenCachePath
}

// cacheToken is a helper function which accepts a token and a file path and
// stores the token in the indicated file.  The contents of the file are
// replaced with the new value.  If the path is blank, the function prints a
// message and returns; other errors result in exiting the process.
func cacheToken(token *oauth2.Token, tokenCachePath string) {
	if tokenCachePath == "" {
		log.Println("The token will not be cached.")
	} else {
		newTokenCacheFile, err := os.OpenFile(tokenCachePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
		if err == nil {
			oauthDebugf("Caching oauth token in %q.", tokenCachePath)
			err = json.NewEncoder(newTokenCacheFile).Encode(token)
			closeFile(newTokenCacheFile)
		}
		if err != nil {
			log.Printf("Unable to cache oauth token: %v", err)
		}
	}
}

// getCacheFileName accepts a file path to the directory containing the token
// cache file and returns an absolute path to the cached token file or an
// error.  If the input path is an empty string, the default path is used; if
// the path is relative, it is prefixed with the platform's user configuration
// directory.  The token file name is appended to the path and the result is
// returned.
func getCacheFileName(tokenCachePath string) (string, error) {
	if tokenCachePath == "" {
		tokenCachePath = defaultTokenCachePath
	}
	if tokenCachePath[0] != '/' {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			log.Printf("unable to determine cache directory: %v", err)
			return "", fmt.Errorf("%w", os.ErrNotExist)
		}
		tokenCachePath = filepath.Join(cacheDir, tokenCachePath)
		if err := os.MkdirAll(tokenCachePath, 0700); err != nil {
			log.Printf("unable to create user cache dir, %q: %v", cacheDir, err)
			return "", fmt.Errorf("%w", os.ErrNotExist)
		}
	}
	return filepath.Join(tokenCachePath, tokenFileName), nil
}

// getCachedTokenSafe is a helper function which reads a cached token from the
// provided file, refreshes it using the provided configuration and context,
// and returns the resulting token. If the token is invalid (e.g., created with
// different credentials), it returns an error instead of fatally exiting.
func getCachedTokenSafe(config *oauth2.Config, cacheFile *os.File, ctx context.Context) (*oauth2.Token, error) {
	token := &oauth2.Token{}
	err := json.NewDecoder(cacheFile).Decode(token)
	if err != nil {
		return nil, fmt.Errorf("unable to parse cached OAuth tokens: %w", err)
	}

	oauthDebugf("[getCachedTokenSafe] Attempting to refresh cached token from %q", cacheFile.Name())
	token, err = config.TokenSource(ctx, token).Token()
	if err != nil {
		// If the error is "unauthorized_client", it likely means the cached token
		// was created with different OAuth client credentials.
		if strings.Contains(err.Error(), "unauthorized_client") {
			oauthDebugf("[getCachedTokenSafe] Cached token is invalid (likely created with different credentials): %v", err)
			return nil, fmt.Errorf("cached token invalid: %w", err)
		}
		return nil, fmt.Errorf("unable to refresh cached token: %w", err)
	}

	oauthDebugf("[getCachedTokenSafe] Successfully refreshed cached token")
	return token, nil
}

// getNewToken is a helper function which prompts the user to use their browser
// to request a new token, obtains the access code when the request is
// redirected to the local listener, exchanges the access code for an access
// token and a refresh token, and returns the token-pair.  The supplied
// configuration is used to access the OAuth 2.0 client configuration to
// generate the access request URL; the redirect URL is modified to include
// a custom port (otherwise, it would default to port 80, which is not
// generally available); and, a random number ("state") is included in the
// request and checked in the redirect to prevent man-in-the-middle attacks.
// After prompting the user, a local listener for the redirect request is
// started, and execution waits for the redirected request which includes the
// access code in the request query parameters.
func getNewToken(config *oauth2.Config, listenerPort string, ctx context.Context) *oauth2.Token {
	stateToken := getStateToken()
	if listenerPort == "" {
		listenerPort = "35355" // Arbitrary value
	}
	config.RedirectURL += ":" + listenerPort
	authURL := config.AuthCodeURL(stateToken, oauth2.AccessTypeOffline)
	fmt.Printf("\nGo to the following link in your browser to authorize access:\n%v\n\n", authURL)

	// Listen for the redirect request, then extract the authorization code
	// from the resulting query params.
	queryParams := redirectListener(config.RedirectURL)
	authCode := getAuthCode(queryParams, stateToken)

	// Exchange the authorization code for an access token and refresh token.
	token, err := config.Exchange(ctx, authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve access token: %v", err)
	}
	return token
}

// getStateToken creates a random state token which is used to validate the
// OAuth redirect request.  The token is the base64-encoded SHA256 hash of the
// current time as a string.
func getStateToken() string {
	h := sha256.New()
	h.Write([]byte(time.Now().Format("20060102150405000000")))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// getAuthCode validates the result of the redirect from the user's
// authorization request, and returns the access code if one is received;
// otherwise it exits the process with a failure.
func getAuthCode(authResp url.Values, stateToken string) string {
	if authResp.Get("state") != stateToken {
		log.Fatalf(
			"Error in authorization state, expected %q, got %q",
			stateToken,
			authResp.Get("state"),
		)
	}
	if authResp.Get("error") != "" {
		log.Fatalf("Error returned from authorization: %s", authResp.Get("error"))
	}
	authCode := authResp.Get("code")
	if authCode == "" {
		log.Fatalf("No authorization code received.")
	}
	return authCode
}

// redirectListener is a helper function used in the creation of the Google API
// client.  It sets up a micro-webserver which listens for a single request to
// the provided URL.  Errors parsing the redirect URL input or starting the
// micro-webserver are logged with Fatalf() which exits the process.
//
// When the request is received, the request is acknowledged, the webserver is
// shut down, and the query parameters of the request (presumably the state
// token and the access code; or an error) are returned.  The request (in the
// user's browser) looks something like this:
//
//	http://localhost/?state=<state_token>&code=<auth_code>&scope=<auth_scopes>
func redirectListener(urlString string) url.Values {
	// This variable is set by the request handler (it is included in the
	// function's closure) and returned after the micro-webserver exits.
	var queryParams url.Values

	// Configure the micro-webserver, add a handler to it for the default
	// route, and start the listener which will serve requests until the
	// server is shut down.
	mux := http.NewServeMux()
	server := http.Server{Addr: getListenAddress(urlString), Handler: mux}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		queryParams = r.URL.Query()
		handleRedirectResponse(w, queryParams)
		// Request the server shutdown in a separate goroutine to allow it to
		// wait for this request to finish processing.
		go requestShutdown(&server)
	})

	// Run the webserver, listening for and dispatching requests, until
	// shutdown is requested.
	if err := server.ListenAndServe(); err != nil {
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Error running redirect listener: %v", err)
		}
	}

	return queryParams
}

// handleRedirectResponse is a helper function which evaluates the redirect
// query parameters and sends an appropriate response to the request.
func handleRedirectResponse(w http.ResponseWriter, queryParams url.Values) {
	msg := `<!doctype html><html lang="en" dir="ltr"><body>`
	if queryParams.Get("code") == "" {
		msg += "<h2>Failure -- no access code received!</h2>"
		if queryParams.Get("error") != "" {
			msg += "<h3>Error:  " + html.EscapeString(queryParams.Get("error")) + "</h3>"
		}
	} else {
		msg += "<h2>Success!  Access code received.</h2>"
	}
	msg += "<p>You may close this browser window.</body></html>"
	_, err := fmt.Fprint(w, msg)
	if err != nil {
		log.Printf("Error writing response to redirect request: %v", err)
	}
}

// requestShutdown is a helper function which requests the server to shut down,
// packaged as a separate function to make it easy to run as a goroutine.
func requestShutdown(server *http.Server) {
	err := server.Shutdown(context.Background())
	if err != nil {
		log.Fatalf("Error shutting down redirect listener: %v", err)
	}
}

// RedirectUrlPattern matches a host (e.g., "localhost" or a FQDN) with an
// optional "http" schema and an optional port.  This is the location provided
// in the OAuth 2.0 client configuration where the authorization flow redirects
// the request after it has been granted or denied.  The schema, if any is
// ignored; path specifications are not supported -- only host (and optionally
// port) should be provided.  The host must resolve to a NIC on the machine
// where this program is being run.
var RedirectUrlPattern = regexp.MustCompile(`^(?:http://)?([^:/]+)(:[0-9]{1,5})$`)

// getListenAddress validates the redirect URL, strips the schema if present,
// sets the address to the host, appends the port if present, and returns the
// result.
func getListenAddress(urlString string) string {
	matches := RedirectUrlPattern.FindStringSubmatch(urlString)
	if matches == nil {
		log.Fatalf("Could not parse redirect URL: %s", urlString)
	}
	address := matches[1]
	if matches[2] != "" {
		address += matches[2]
	}
	return address
}
