package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/plimsoll-protocol/plimsoll-indexer/internal/coverage"
	"github.com/plimsoll-protocol/plimsoll-indexer/internal/store"
	"github.com/plimsoll-protocol/plimsoll-indexer/internal/webhook"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const sac = "CCHPT4TEJDPZQDSUVW3NP6A35ROGV4WEFZKT45SKWYCFZSUB7R5HIM7S"

// breakdownHash is what a SHA-256 breakdown hash looks like: 64 hex characters.
var breakdownHash = strings.Repeat("a", 64)

// unlistedSAC is a well-formed contract strkey that fake holds no asset for.
func unlistedSAC(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = 0x01
	s, err := strkey.Encode(strkey.VersionByteContract, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fake struct {
	assets  []coverage.Asset
	supply  map[string][]coverage.Supply
	reports map[string][]fakeReport
	bodies  map[string][]byte
}

func (f *fake) Ping(context.Context) error { return nil }
func (f *fake) ListAssets(context.Context) ([]coverage.Asset, error) {
	return f.assets, nil
}
func (f *fake) GetAsset(_ context.Context, s string) (coverage.Asset, error) {
	for _, a := range f.assets {
		if a.SAC == s {
			return a, nil
		}
	}
	return coverage.Asset{}, store.ErrNotFound
}
func (f *fake) LatestSupply(_ context.Context, s string) (*coverage.Supply, error) {
	if list := f.supply[s]; len(list) > 0 {
		return &list[0], nil
	}
	return nil, nil
}
func (f *fake) SupplyHistory(_ context.Context, s string, limit int, before *uint32) ([]coverage.Supply, *uint32, error) {
	list := f.supply[s]
	var filtered []coverage.Supply
	for _, sp := range list {
		if before != nil && sp.Ledger >= *before {
			continue
		}
		filtered = append(filtered, sp)
	}
	var nextBefore *uint32
	if len(filtered) == limit+1 {
		filtered = filtered[:limit]
		v := filtered[limit-1].Ledger
		nextBefore = &v
	}
	return filtered, nextBefore, nil
}
func (f *fake) LatestReports(_ context.Context, s string) (map[uint32]coverage.Report, error) {
	out := make(map[uint32]coverage.Report)
	for _, rep := range f.reports[s] {
		if cur, ok := out[rep.Tier]; !ok || rep.AsOf.After(cur.AsOf) {
			out[rep.Tier] = rep.Report
		}
	}
	return out, nil
}
func (f *fake) ReportHistory(_ context.Context, s string, limit int, before *uint32) ([]coverage.Report, *uint32, error) {
	list := f.reports[s]
	var kept []fakeReport
	for _, rep := range list {
		if before != nil && rep.PostedLedger >= *before {
			continue
		}
		kept = append(kept, rep)
	}
	var nextBefore *uint32
	out := make([]coverage.Report, 0, len(kept))
	max := len(kept)
	if max == limit+1 {
		max = limit
		v := kept[limit-1].PostedLedger
		nextBefore = &v
	}
	for i := 0; i < max; i++ {
		out = append(out, kept[i].Report)
	}
	return out, nextBefore, nil
}
func (f *fake) Reporters(context.Context) ([]coverage.Reporter, error) {
	return []coverage.Reporter{{Address: "GAUD", Role: coverage.RoleAuditor, Name: "Acme"}}, nil
}
func (f *fake) Breakdown(_ context.Context, h string) ([]byte, error) {
	if b, ok := f.bodies[h]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}
func (f *fake) Progress(context.Context, string) (store.Progress, error) {
	return store.Progress{
		Cursor:        "c",
		ScannedLedger: 42,
		EventLedger:   40,
		EventAt:       now.Add(-90 * time.Second),
	}, nil
}

type webhookManagerFake struct {
	webhooks []store.Webhook
}

func (f *webhookManagerFake) CreateWebhook(_ context.Context, url, hash string, ciphertext []byte) (store.Webhook, error) {
	item := store.Webhook{
		ID: int64(len(f.webhooks) + 1), URL: url, SecretHash: hash,
		SecretCiphertext: ciphertext, CreatedAt: now,
	}
	f.webhooks = append(f.webhooks, item)
	return item, nil
}

func (f *webhookManagerFake) ListWebhooks(context.Context) ([]store.Webhook, error) {
	return f.webhooks, nil
}

func (f *webhookManagerFake) DeleteWebhook(_ context.Context, id int64) (bool, error) {
	for i, item := range f.webhooks {
		if item.ID == id {
			f.webhooks = append(f.webhooks[:i], f.webhooks[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func server() *httptest.Server {
	return serverWithWebhooks(nil, nil, "")
}

func serverWithWebhooks(manager WebhookManager, codec *webhook.SecretCodec, token string) *httptest.Server {
	f := &fake{
		assets: []coverage.Asset{{SAC: sac, Code: "PUSD", Issuer: "GISS"}},
		supply: map[string][]coverage.Supply{
			sac: {
				{Amount: big.NewInt(1_000_0000000), Ledger: 100, PostedAt: supplyNow},
				{Amount: big.NewInt(9_00000000), Ledger: 90, PostedAt: supplyNow.Add(-time.Hour)},
				{Amount: big.NewInt(8_00000000), Ledger: 80, PostedAt: supplyNow.Add(-2 * time.Hour)},
				{Amount: big.NewInt(7_00000000), Ledger: 70, PostedAt: supplyNow.Add(-3 * time.Hour)},
			},
		},
		reports: map[string][]fakeReport{
			sac: {
				{Report: coverage.Report{Tier: coverage.TierIssuerSigned, Amount: big.NewInt(1_020_0000000),
					AsOf: now.Add(-2 * time.Hour), PostedAt: now.Add(-2 * time.Hour), Reporter: "GISS", DocURI: "https://x"},
					PostedLedger: 50},
				{Report: coverage.Report{Tier: coverage.TierIssuerSigned, Amount: big.NewInt(1_010_0000000),
					AsOf: now.Add(-4 * time.Hour), PostedAt: now.Add(-4 * time.Hour), Reporter: "GISS", DocURI: "https://x"},
					PostedLedger: 40},
				{Report: coverage.Report{Tier: coverage.TierTranscribed, Amount: big.NewInt(1_005_0000000),
					AsOf: now.Add(-6 * time.Hour), PostedAt: now.Add(-6 * time.Hour), Reporter: "GISS", DocURI: "https://x"},
					PostedLedger: 30},
			},
		},
		bodies: map[string][]byte{breakdownHash: []byte(`{"total":"1"}`)},
	}
	s := &Server{
		Store:             f,
		WebhookStore:      manager,
		WebhookCodec:      codec,
		WebhookAdminToken: token,
		CORS:              []string{"*"},
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:               func() time.Time { return now },
	}
	return httptest.NewServer(s.Handler())
}

func get(t *testing.T, srv *httptest.Server, path string, want int) map[string]any {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s: status %d want %d", path, resp.StatusCode, want)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestGetAssetCoverage(t *testing.T) {
	srv := server()
	defer srv.Close()
	body := get(t, srv, "/v1/assets/"+sac, 200)
	cov := body["coverage"].(map[string]any)
	if cov["bps"].(float64) != 10_200 || cov["ratio"] != "102.00%" || cov["tier"] != "issuer_signed" {
		t.Fatalf("coverage %+v", cov)
	}
	if cov["report_age_seconds"].(float64) != 7200 {
		t.Errorf("age %v", cov["report_age_seconds"])
	}
	if body["supply"].(map[string]any)["amount"] != "10000000000" {
		t.Errorf("supply %+v", body["supply"])
	}
}

func TestMinTierExcludesWeakerReports(t *testing.T) {
	srv := server()
	defer srv.Close()
	body := get(t, srv, "/v1/assets/"+sac+"?min_tier=3", 200)
	if body["coverage"] != nil {
		t.Fatalf("expected no coverage at auditor tier, got %+v", body["coverage"])
	}
	get(t, srv, "/v1/assets/"+sac+"?min_tier=9", 400)
}

func TestListAndMisc(t *testing.T) {
	srv := server()
	defer srv.Close()
	list := get(t, srv, "/v1/assets", 200)
	if len(list["assets"].([]any)) != 1 {
		t.Fatal("expected one asset")
	}
	get(t, srv, "/v1/assets/CNOPE", 400)
	rep := get(t, srv, "/v1/reporters", 200)
	if rep["reporters"].([]any)[0].(map[string]any)["role"] != "auditor" {
		t.Fatal("role name missing")
	}
	h := get(t, srv, "/healthz", 200)
	if h["ingested_to_ledger"].(float64) != 42 {
		t.Fatal("health missing ledger")
	}
	// A scan far ahead of the last stored event is the shape of silently
	// broken ingestion, so health has to report both.
	if h["last_event_ledger"].(float64) != 40 {
		t.Errorf("last_event_ledger = %v", h["last_event_ledger"])
	}
	if h["last_event_age_seconds"].(float64) != 90 {
		t.Errorf("last_event_age_seconds = %v", h["last_event_age_seconds"])
	}
	get(t, srv, "/v1/assets/"+sac+"/supply?limit=0", 400)
	get(t, srv, "/v1/assets/"+sac+"/reports", 200)
}

func TestSupplyHistoryPaging(t *testing.T) {
	srv := server()
	defer srv.Close()
	var before *uint32
	var seen []uint32
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("/v1/assets/%s/supply?limit=2", sac)
		if before != nil {
			path += fmt.Sprintf("&before=%d", *before)
		}
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Supply     []any   `json:"supply"`
			NextBefore *uint32 `json:"next_before"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, s := range page.Supply {
			m := s.(map[string]any)
			seen = append(seen, uint32(m["ledger"].(float64)))
		}
		if page.NextBefore == nil {
			break
		}
		before = page.NextBefore
	}
	want := []uint32{100, 90, 80, 70}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("supply history = %v, want %v", seen, want)
	}
}

func TestReportHistoryPaging(t *testing.T) {
	srv := server()
	defer srv.Close()
	var before *uint32
	var seen []string
	pages := 0
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("/v1/assets/%s/reports?limit=2", sac)
		if before != nil {
			path += fmt.Sprintf("&before=%d", *before)
		}
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Reports    []any   `json:"reports"`
			NextBefore *uint32 `json:"next_before"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		pages++
		for _, r := range page.Reports {
			m := r.(map[string]any)
			seen = append(seen, m["as_of"].(string))
		}
		if page.NextBefore == nil {
			break
		}
		before = page.NextBefore
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 reports across pages, got %d: %v", len(seen), seen)
	}
	if pages < 2 {
		t.Fatalf("expected multiple pages, got %d", pages)
	}
}

func TestPagingInvalidBefore(t *testing.T) {
	srv := server()
	defer srv.Close()
	get(t, srv, "/v1/assets/"+sac+"/supply?before=abc", 400)
	get(t, srv, "/v1/assets/"+sac+"/reports?before=abc", 400)
	get(t, srv, "/v1/assets/"+sac+"/supply?before=9999999999", 400)
}

func newServer(supply *coverage.Supply, reports map[uint32]coverage.Report) *httptest.Server {
	f := &fake{
		assets: []coverage.Asset{{SAC: sac, Code: "PUSD", Issuer: "GISS"}},
		bodies: map[string][]byte{},
	}
	if supply != nil {
		f.supply = map[string][]coverage.Supply{sac: {*supply}}
	}
	if reports != nil {
		list := make([]fakeReport, 0, len(reports))
		for _, r := range reports {
			list = append(list, fakeReport{Report: r})
		}
		f.reports = map[string][]fakeReport{sac: list}
	}
	s := &Server{
		Store: f,
		CORS:  []string{"*"},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return now },
	}
	return httptest.NewServer(s.Handler())
}

func TestCovered(t *testing.T) {
	sp := &coverage.Supply{Amount: big.NewInt(100), PostedAt: now}
	rep := coverage.Report{Tier: coverage.TierTranscribed, Amount: big.NewInt(101), AsOf: now}
	day := 24 * time.Hour

	srv := newServer(sp, map[uint32]coverage.Report{coverage.TierTranscribed: rep})
	defer srv.Close()
	body := get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=10000&max_age=24h", sac), 200)
	if body["covered"] != true || body["reason"] != "covered" {
		t.Fatalf("covered: %v", body)
	}

	srv = newServer(nil, map[uint32]coverage.Report{coverage.TierTranscribed: rep})
	defer srv.Close()
	body = get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0&max_age=24h", sac), 200)
	if body["covered"] != false || body["reason"] != "no_supply" {
		t.Fatalf("no_supply: %v", body)
	}

	srv = newServer(sp, map[uint32]coverage.Report{})
	defer srv.Close()
	body = get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0&max_age=24h", sac), 200)
	if body["covered"] != false || body["reason"] != "no_report" {
		t.Fatalf("no_report: %v", body)
	}

	oldSp := &coverage.Supply{Amount: big.NewInt(100), PostedAt: now.Add(-2 * day)}
	srv = newServer(oldSp, map[uint32]coverage.Report{coverage.TierTranscribed: rep})
	defer srv.Close()
	body = get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0&max_age=24h", sac), 200)
	if body["covered"] != false || body["reason"] != "stale_supply" {
		t.Fatalf("stale_supply: %v", body)
	}

	oldRep := coverage.Report{Tier: coverage.TierTranscribed, Amount: big.NewInt(101), AsOf: now.Add(-2 * day)}
	srv = newServer(sp, map[uint32]coverage.Report{coverage.TierTranscribed: oldRep})
	defer srv.Close()
	body = get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0&max_age=24h", sac), 200)
	if body["covered"] != false || body["reason"] != "stale_report" {
		t.Fatalf("stale_report: %v", body)
	}

	srv = newServer(sp, map[uint32]coverage.Report{coverage.TierTranscribed: rep})
	defer srv.Close()
	body = get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=10200&max_age=24h", sac), 200)
	if body["covered"] != false || body["reason"] != "under_backed" {
		t.Fatalf("under_backed: %v", body)
	}
}

func TestCoveredInvalidParams(t *testing.T) {
	srv := newServer(nil, nil)
	defer srv.Close()
	get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?max_age=24h", sac), 400)
	get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0", sac), 400)
	get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=abc&max_age=24h", sac), 400)
	get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0&max_age=abc", sac), 400)
	get(t, srv, fmt.Sprintf("/v1/assets/%s/covered?min_bps=0&max_age=0", sac), 400)
}

func TestBreakdownServedVerbatim(t *testing.T) {
	srv := server()
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/breakdowns/" + breakdownHash)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != `{"total":"1"}` {
		t.Fatalf("body %q", b)
	}
	get(t, srv, "/v1/breakdowns/"+strings.Repeat("0", 64), 404)
}

// Every path id is checked before it reaches storage: malformed ids are a
// client error, while well-formed but unlisted ids are a missing resource.
func TestMalformedPathIDs(t *testing.T) {
	srv := server()
	defer srv.Close()
	for _, path := range []string{
		"/v1/assets/not-a-sac",
		"/v1/assets/CNOPE",
		"/v1/assets/not-a-sac/supply",
		"/v1/assets/not-a-sac/reports",
	} {
		body := get(t, srv, path, 400)
		if body["error"] == nil {
			t.Errorf("%s: expected an error message, got %+v", path, body)
		}
	}
	for _, hash := range []string{"abc", strings.Repeat("a", 63), strings.Repeat("z", 64)} {
		body := get(t, srv, "/v1/breakdowns/"+hash, 400)
		if body["error"] == nil {
			t.Errorf("hash %q: expected an error message, got %+v", hash, body)
		}
	}
}

func TestUnlistedPathIDs(t *testing.T) {
	srv := server()
	defer srv.Close()
	unknown := unlistedSAC(t)
	for _, path := range []string{
		"/v1/assets/" + unknown,
		"/v1/assets/" + unknown + "/supply",
		"/v1/assets/" + unknown + "/reports",
	} {
		body := get(t, srv, path, 404)
		if body["error"] != "asset not listed" {
			t.Errorf("%s: body %+v", path, body)
		}
	}
}

// A listed asset keeps answering 200 from both history endpoints.
func TestListedAssetHistory(t *testing.T) {
	srv := server()
	defer srv.Close()
	if got := get(t, srv, "/v1/assets/"+sac+"/supply", 200)["supply"].([]any); len(got) != 1 {
		t.Fatalf("supply history %+v", got)
	}
	if got := get(t, srv, "/v1/assets/"+sac+"/reports", 200)["reports"].([]any); len(got) != 1 {
		t.Fatalf("report history %+v", got)
	}
}

func TestCORSHeader(t *testing.T) {
	srv := server()
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/assets", nil)
	req.Header.Set("Origin", "https://plimsoll.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status %d, header %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestOpenAPISpec(t *testing.T) {
	srv := server()
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "openapi: 3.1.0") {
		t.Fatalf("expected openapi 3.1.0 spec, got: %s", string(body))
	}
}

func TestWebhookSecretReturnedOnlyAtCreation(t *testing.T) {
	manager := &webhookManagerFake{}
	codec, err := webhook.NewSecretCodec(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	srv := serverWithWebhooks(manager, codec, "admin-token")
	defer srv.Close()

	request := func(method, path, body string, authorized bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer admin-token")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	unauthorized := request(http.MethodPost, "/v1/webhooks", `{"url":"https://receiver.example/hook"}`, false)
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized create status = %d, want 401", unauthorized.StatusCode)
	}

	created := request(http.MethodPost, "/v1/webhooks", `{"url":"https://receiver.example/hook"}`, true)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", created.StatusCode)
	}
	var response struct {
		ID     int64  `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(created.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Secret == "" || len(manager.webhooks) != 1 {
		t.Fatalf("creation response %q, stored webhooks %d", response.Secret, len(manager.webhooks))
	}
	stored := manager.webhooks[0]
	if stored.SecretHash != webhook.SecretHash(response.Secret) {
		t.Fatal("stored secret hash does not match the one-time returned secret")
	}
	decrypted, err := codec.Decrypt(stored.SecretCiphertext)
	if err != nil || decrypted != response.Secret {
		t.Fatalf("stored signing secret = %q, %v", decrypted, err)
	}

	list := request(http.MethodGet, "/v1/webhooks", "", true)
	defer list.Body.Close()
	listBody, err := io.ReadAll(list.Body)
	if err != nil {
		t.Fatal(err)
	}
	if list.StatusCode != http.StatusOK || strings.Contains(string(listBody), response.Secret) ||
		strings.Contains(string(listBody), "secret_hash") || strings.Contains(string(listBody), "secret_ciphertext") {
		t.Fatalf("list response disclosed secret or failed: status=%d body=%s", list.StatusCode, listBody)
	}
}
