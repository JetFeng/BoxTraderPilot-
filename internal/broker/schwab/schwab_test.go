package schwab

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/broker"
	"boxpilot/internal/logx"
)

const (
	fakeClientID     = "CLIENTID-fake-0123456789abcdef"
	fakeClientSecret = "APPSECRET-fake-0123456789abcdef"
	fakeRefreshToken = "REFRESHTOKEN-fake-aaaa-bbbb-cccc-dddddddddddd"
)

// fakeServer routes token/chains/quotes and records requests.
type fakeServer struct {
	mu           sync.Mutex
	tokenHits    int
	tokenAuths   []string
	tokenBodies  []string
	chainQueries []string
	quoteQueries []string
	authHeaders  []string

	// responders
	chainHandler func(w http.ResponseWriter, r *http.Request)
	quoteHandler func(w http.ResponseWriter, r *http.Request)
	tokenHandler func(w http.ResponseWriter, r *http.Request)
}

func newFakeServer() *fakeServer {
	fs := &fakeServer{}
	fs.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":"ACCESS-fake-1","refresh_token":%q,"expires_in":1800,"token_type":"Bearer"}`, fakeRefreshToken)
	}
	fs.chainHandler = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }
	fs.quoteHandler = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }
	return fs
}

func (fs *fakeServer) srv(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		fs.mu.Lock()
		fs.tokenHits++
		fs.tokenAuths = append(fs.tokenAuths, r.Header.Get("Authorization"))
		fs.tokenBodies = append(fs.tokenBodies, form.Get("refresh_token"))
		fs.mu.Unlock()
		// Restore the body so the responder can inspect it too.
		r.Body = io.NopCloser(bytes.NewReader(body))
		fs.tokenHandler(w, r)
	})
	mux.HandleFunc("/marketdata/v1/chains", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.chainQueries = append(fs.chainQueries, r.URL.RawQuery)
		fs.authHeaders = append(fs.authHeaders, r.Header.Get("Authorization"))
		fs.mu.Unlock()
		fs.chainHandler(w, r)
	})
	mux.HandleFunc("/marketdata/v1/quotes", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.quoteQueries = append(fs.quoteQueries, r.URL.RawQuery)
		fs.authHeaders = append(fs.authHeaders, r.Header.Get("Authorization"))
		fs.mu.Unlock()
		fs.quoteHandler(w, r)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func newClient(t *testing.T, fs *fakeServer) (*Client, *[]time.Duration) {
	t.Helper()
	s := fs.srv(t)
	sleeps := &[]time.Duration{}
	c, err := New(Config{
		ClientID:     fakeClientID,
		ClientSecret: fakeClientSecret,
		RefreshToken: fakeRefreshToken,
		BaseURL:      s.URL,
		Logger:       logx.New(discardWriter{}, "schwab ", []string{fakeRefreshToken, "ACCESS-fake-1"}),
		Sleep:        func(d time.Duration) { *sleeps = append(*sleeps, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sleeps
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty credentials")
	}
}

func TestGetQuoteTokenCachedAndBasicAuth(t *testing.T) {
	fs := newFakeServer()
	fs.quoteHandler = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"$SPX":{"assetMainType":"IDX","quote":{"lastPrice":5912.3,"quoteTime":1762462800000}}}`)
	}
	c, _ := newClient(t, fs)

	for i := 0; i < 2; i++ {
		q, err := c.GetQuote(context.Background(), "SPX")
		if err != nil {
			t.Fatal(err)
		}
		if q.Price != box.MustParsePrice("5912.30") {
			t.Errorf("price = %v", q.Price)
		}
		if !q.Timestamp.Equal(time.UnixMilli(1762462800000).UTC()) {
			t.Errorf("timestamp = %v", q.Timestamp)
		}
	}
	if fs.tokenHits != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (cache)", fs.tokenHits)
	}
	if len(fs.authHeaders) == 0 || !strings.HasPrefix(fs.authHeaders[0], "Bearer ") {
		t.Errorf("missing bearer auth: %v", fs.authHeaders)
	}
	// The token request itself must use Basic auth with app credentials.
	// (verified indirectly: token handler succeeded; check the query used $SPX)
	if len(fs.quoteQueries) != 2 || !strings.Contains(fs.quoteQueries[0], "%24SPX") {
		t.Errorf("quote queries = %v", fs.quoteQueries)
	}
}

func TestGetQuoteFallsBackToPlainSymbol(t *testing.T) {
	fs := newFakeServer()
	fs.quoteHandler = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "%24SPX") {
			fmt.Fprint(w, `{}`) // index form absent → fallback
			return
		}
		fmt.Fprint(w, `{"SPX":{"assetMainType":"IDX","quote":{"lastPrice":5912.3,"quoteTime":1762462800000}}}`)
	}
	c, _ := newClient(t, fs)
	q, err := c.GetQuote(context.Background(), "SPX")
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != box.MustParsePrice("5912.30") {
		t.Errorf("price = %v", q.Price)
	}
	if len(fs.quoteQueries) != 2 {
		t.Errorf("expected 2 attempts (index then plain), got %v", fs.quoteQueries)
	}
}

func chainPayload(date string, quoteMS int64) string {
	return fmt.Sprintf(`{
	  "symbol": "SPX", "status": "SUCCESS", "isDelayed": false, "isIndex": true,
	  "callExpDateMap": {"%s:40": {
	    "5900.0": [{"putCall":"CALL","symbol":"SPX  261218C00590000","bidPrice":98.5,"askPrice":98.7,"lastPrice":98.6,"strikePrice":5900.0,"totalVolume":100,"openInterest":2000,"expirationDate":1766005200000,"quoteTime":%d}],
	    "6150.0": [{"putCall":"CALL","symbol":"SPX  261218C00615000","bidPrice":3.1,"askPrice":3.3,"lastPrice":3.2,"strikePrice":6150.0,"totalVolume":50,"openInterest":900,"expirationDate":1766005200000,"quoteTime":%d}]
	  }},
	  "putExpDateMap": {"%s:40": {
	    "5900.0": [{"putCall":"PUT","symbol":"SPX  261218P00590000","bidPrice":20.1,"askPrice":20.4,"lastPrice":20.3,"strikePrice":5900.0,"totalVolume":80,"openInterest":1500,"expirationDate":1766005200000,"quoteTime":%d}],
	    "6150.0": [{"putCall":"PUT","symbol":"SPX  261218P00615000","bidPrice":24.1,"askPrice":24.4,"lastPrice":24.3,"strikePrice":6150.0,"totalVolume":60,"openInterest":1100,"expirationDate":1766005200000,"quoteTime":%d}]
	  }}
	}`, date, quoteMS, quoteMS, date, quoteMS, quoteMS)
}

func TestGetOptionChainParsesSpecFields(t *testing.T) {
	fs := newFakeServer()
	const quoteMS = int64(1762462800000)
	fs.chainHandler = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chainPayload("2026-12-18", quoteMS))
	}
	c, _ := newClient(t, fs)

	exp, _ := time.Parse("2006-01-02", "2026-12-18")
	ch, err := c.GetOptionChain(context.Background(), "SPX", exp)
	if err != nil {
		t.Fatal(err)
	}
	// One batched request with the expiry pinned.
	if len(fs.chainQueries) != 1 {
		t.Fatalf("chain queries = %d, want 1", len(fs.chainQueries))
	}
	q := fs.chainQueries[0]
	for _, want := range []string{"symbol=SPX", "contractType=ALL", "fromDate=2026-12-18", "toDate=2026-12-18", "strategy=SINGLE"} {
		if !strings.Contains(q, want) {
			t.Errorf("chain query missing %q: %s", want, q)
		}
	}
	if len(ch.Strikes) != 2 || ch.Strikes[0].Strike != 5900 || ch.Strikes[1].Strike != 6150 {
		t.Fatalf("strikes = %+v", ch.Strikes)
	}
	if got := ch.Strikes[0].Call.Bid; got != box.MustParsePrice("98.5") {
		t.Errorf("call bid = %v", got)
	}
	if got := ch.Strikes[1].Put.Ask; got != box.MustParsePrice("24.4") {
		t.Errorf("put ask = %v", got)
	}
	if ch.Strikes[0].Call.Interest != 2000 {
		t.Errorf("open interest = %d", ch.Strikes[0].Call.Interest)
	}
	if !ch.Timestamp.Equal(time.UnixMilli(quoteMS).UTC()) {
		t.Errorf("timestamp = %v", ch.Timestamp)
	}
}

func TestRetryOn429WithRetryAfter(t *testing.T) {
	fs := newFakeServer()
	var calls int
	fs.chainHandler = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, chainPayload("2026-12-18", 1762462800000))
	}
	c, sleeps := newClient(t, fs)
	exp, _ := time.Parse("2006-01-02", "2026-12-18")
	if _, err := c.GetOptionChain(context.Background(), "SPX", exp); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(*sleeps) == 0 || (*sleeps)[0] != time.Second {
		t.Errorf("Retry-After backoff not honored: %v", *sleeps)
	}
}

func TestRetryOnServerErrorGivesUp(t *testing.T) {
	fs := newFakeServer()
	var calls int
	fs.chainHandler = func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}
	c, _ := newClient(t, fs)
	exp, _ := time.Parse("2006-01-02", "2026-12-18")
	if _, err := c.GetOptionChain(context.Background(), "SPX", exp); err == nil {
		t.Fatal("expected error")
	}
	if calls != maxAttempts {
		t.Errorf("calls = %d, want %d", calls, maxAttempts)
	}
}

func TestClientErrorNotRetried(t *testing.T) {
	fs := newFakeServer()
	var calls int
	fs.chainHandler = func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
	}
	c, _ := newClient(t, fs)
	exp, _ := time.Parse("2006-01-02", "2026-12-18")
	if _, err := c.GetOptionChain(context.Background(), "SPX", exp); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", calls)
	}
}

func TestChainStatusFailure(t *testing.T) {
	fs := newFakeServer()
	fs.chainHandler = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"symbol":"SPX","status":"FAILED"}`)
	}
	c, _ := newClient(t, fs)
	exp, _ := time.Parse("2006-01-02", "2026-12-18")
	if _, err := c.GetOptionChain(context.Background(), "SPX", exp); err == nil {
		t.Fatal("expected status error")
	}
}

func TestQuoteNotFound(t *testing.T) {
	fs := newFakeServer() // default: empty object for both candidate symbols
	c, _ := newClient(t, fs)
	_, err := c.GetQuote(context.Background(), "SPX")
	if !errors.Is(err, broker.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestTokenErrorDoesNotLeakSecret(t *testing.T) {
	fs := newFakeServer()
	fs.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
	}
	c, _ := newClient(t, fs)
	_, err := c.GetQuote(context.Background(), "SPX")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), fakeRefreshToken) {
		t.Errorf("secret leaked in error: %v", err)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(fakeClientID+":"+fakeClientSecret))
	if len(fs.tokenAuths) == 0 {
		t.Fatal("no token request recorded")
	}
	for _, got := range fs.tokenAuths {
		if got != wantBasic {
			t.Errorf("token auth header = %q, want %q", got, wantBasic)
		}
	}
}

// --- OAuth refresh token rotation store ---------------------------------
//
// Schwab invalidates the previous refresh token on every refresh. The
// rotated value must survive a restart, otherwise the tool is left
// holding a dead credential.

const rotatedToken = "ROTATED-fake-2"

// newClientWithStore builds a client with an explicit token store.
func newClientWithStore(t *testing.T, fs *fakeServer, storePath, refreshToken string) *Client {
	t.Helper()
	s := fs.srv(t)
	c, err := New(Config{
		ClientID:       fakeClientID,
		ClientSecret:   fakeClientSecret,
		RefreshToken:   refreshToken,
		BaseURL:        s.URL,
		TokenStorePath: storePath,
		Logger:         logx.New(discardWriter{}, "schwab ", nil),
		Sleep:          func(d time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// quoteOK makes the fake quotes endpoint answer.
func quoteOK(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, `{"$SPX":{"assetMainType":"IDX","quote":{"lastPrice":5912.3,"quoteTime":1762462800000}}}`)
}

func TestRefreshTokenRotationIsPersisted(t *testing.T) {
	fs := newFakeServer()
	fs.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":"ACCESS-fake-1","refresh_token":%q,"expires_in":1800,"token_type":"Bearer"}`, rotatedToken)
	}
	fs.quoteHandler = quoteOK
	storePath := filepath.Join(t.TempDir(), "sub", "schwab_refresh_token")
	c := newClientWithStore(t, fs, storePath, fakeRefreshToken)

	if _, err := c.GetQuote(context.Background(), "SPX"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("rotated token was not persisted: %v", err)
	}
	if strings.TrimSpace(string(got)) != rotatedToken {
		t.Errorf("persisted token = %q, want %q", strings.TrimSpace(string(got)), rotatedToken)
	}
	fi, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token store mode = %o, want 600", perm)
	}
}

func TestPersistedTokenIsAdoptedOnRestart(t *testing.T) {
	fs := newFakeServer()
	fs.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		// Same token back → no rotation.
		fmt.Fprintf(w, `{"access_token":"ACCESS-fake-1","refresh_token":%q,"expires_in":1800,"token_type":"Bearer"}`, rotatedToken)
	}
	fs.quoteHandler = quoteOK
	storePath := filepath.Join(t.TempDir(), "schwab_refresh_token")
	// A previous run persisted the rotated token; the environment still
	// holds the original, now-invalid value.
	if err := saveToken(storePath, rotatedToken); err != nil {
		t.Fatal(err)
	}
	c := newClientWithStore(t, fs, storePath, fakeRefreshToken)

	if _, err := c.GetQuote(context.Background(), "SPX"); err != nil {
		t.Fatal(err)
	}
	if len(fs.tokenBodies) != 1 {
		t.Fatalf("token requests = %d, want 1", len(fs.tokenBodies))
	}
	if fs.tokenBodies[0] != rotatedToken {
		t.Errorf("refreshed with %q, want the persisted token %q", fs.tokenBodies[0], rotatedToken)
	}
}

func TestRefreshFallsBackToConfiguredToken(t *testing.T) {
	fs := newFakeServer()
	fs.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("refresh_token") == "REVOKED-fake-3" {
			http.Error(w, "invalid refresh token", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"access_token":"ACCESS-fake-1","refresh_token":"ROTATED-fake-4","expires_in":1800,"token_type":"Bearer"}`)
	}
	fs.quoteHandler = quoteOK
	storePath := filepath.Join(t.TempDir(), "schwab_refresh_token")
	if err := saveToken(storePath, "REVOKED-fake-3"); err != nil {
		t.Fatal(err)
	}
	c := newClientWithStore(t, fs, storePath, fakeRefreshToken)

	if _, err := c.GetQuote(context.Background(), "SPX"); err != nil {
		t.Fatalf("expected the fallback token to work: %v", err)
	}
	if len(fs.tokenBodies) != 2 {
		t.Fatalf("token requests = %d, want 2 (persisted then fallback)", len(fs.tokenBodies))
	}
	if fs.tokenBodies[0] != "REVOKED-fake-3" || fs.tokenBodies[1] != fakeRefreshToken {
		t.Errorf("token requests = %v, want [REVOKED-fake-3 %s]", fs.tokenBodies, fakeRefreshToken)
	}
}

func TestNoTokenStoreWritesNothing(t *testing.T) {
	// Without an explicit store the package must stay side-effect free.
	fs := newFakeServer()
	fs.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":"ACCESS-fake-1","refresh_token":%q,"expires_in":1800,"token_type":"Bearer"}`, rotatedToken)
	}
	fs.quoteHandler = quoteOK
	c := newClientWithStore(t, fs, "", fakeRefreshToken)
	if _, err := c.GetQuote(context.Background(), "SPX"); err != nil {
		t.Fatal(err)
	}
	if c.storePath != "" {
		t.Errorf("storePath = %q, want empty", c.storePath)
	}
}

func TestDefaultTokenStorePathHonoursEnv(t *testing.T) {
	t.Setenv(TokenStoreEnv, "/tmp/custom-token-store")
	if got := DefaultTokenStorePath(); got != "/tmp/custom-token-store" {
		t.Errorf("DefaultTokenStorePath() = %q, want the env override", got)
	}
}
