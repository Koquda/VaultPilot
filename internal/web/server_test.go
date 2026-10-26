package web

import (
	"bytes"
	"encoding/csv"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Koquda/VaultPilot/internal/store"
)

const fixture = `date,provider,service,cost,currency
2026-10-01,aws,Compute,100,USD
2026-10-02,aws,Compute,100,USD
2026-10-03,aws,Compute,100,USD
2026-10-04,aws,Compute,100,USD
2026-10-05,aws,Compute,100,USD
2026-10-06,aws,Compute,100,USD
2026-10-07,aws,Compute,100,USD
2026-10-08,aws,Compute,250,USD
2026-10-08,aws,Credit,-25,USD
2026-10-08,gcp,"Database, managed",40,USD
`

func setup(t *testing.T) (*Server, *store.Store, *http.Cookie) {
	t.Helper()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	st, e := store.Open(filepath.Join(t.TempDir(), "workspace.json"), now)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	s, e := New(st, "127.0.0.1:8080")
	if e != nil {
		t.Fatal(e)
	}
	s.now = func() time.Time { return now }
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("initial status %d: %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing browser form cookie")
	}
	if !strings.Contains(w.Body.String(), "DEMO DATA") || strings.Contains(w.Body.String(), "<script") {
		t.Fatal("demo page must be explicit and script-free")
	}
	return s, st, cookies[0]
}
func post(s *Server, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	if values == nil {
		values = url.Values{}
	}
	if cookie != nil {
		values.Set("csrf", cookie.Value)
	}
	if path == "/budget" && !values.Has("snapshot") {
		form := get(s, "/settings").Body.String()
		for _, field := range []string{"snapshot", "currency"} {
			match := regexp.MustCompile(`name="` + field + `" value="([^"]+)"`).FindStringSubmatch(form)
			if len(match) == 2 {
				values.Set(field, match[1])
			}
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080"+path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func get(s *Server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil))
	return w
}
func prepare(t *testing.T, s *Server, cookie *http.Cookie, text string) string {
	t.Helper()
	w := post(s, "/imports/preview", url.Values{"csv": {text}, "from": {"2026-10-01"}, "through": {"2026-10-08"}, "complete": {"yes"}}, cookie)
	if w.Code != 200 {
		t.Fatalf("preview status %d: %s", w.Code, w.Body)
	}
	match := regexp.MustCompile(`name="token" value="([a-f0-9]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("preview missing confirmation form")
	}
	return match[1]
}
func TestImportPreviewAndDashboard(t *testing.T) {
	s, st, cookie := setup(t)
	before := st.Read().Active.ID
	token := prepare(t, s, cookie, fixture)
	if st.Read().Active.ID != before {
		t.Fatal("preview replaced data before confirmation")
	}
	w := post(s, "/imports/apply", url.Values{"token": {token}}, cookie)
	if w.Code != 303 {
		t.Fatalf("apply %d %s", w.Code, w.Body)
	}
	w = get(s, "/")
	for _, value := range []string{"$965.00", "$3,889.29", "IMPORTED DATA", "Database, managed", "+150%", "New spend"} {
		if !strings.Contains(html.UnescapeString(w.Body.String()), value) {
			t.Errorf("dashboard missing %q", value)
		}
	}
	if strings.Contains(w.Body.String(), "ZgotmplZ") {
		t.Fatal("template rejected CSS or URL")
	}
	if got := st.Read(); got.Demo || len(got.Active.Rows) != 10 {
		t.Fatal("import did not replace demo snapshot")
	}
	if w := post(s, "/imports/apply", url.Values{"token": {token}}, cookie); w.Code != 409 {
		t.Fatal("consumed preview should not apply again")
	}
	w = get(s, "/?month=2026-10&provider=gcp")
	if !strings.Contains(w.Body.String(), `href="/transactions?month=2026-10&amp;provider=gcp`) {
		t.Fatal("navigation loses the provider filter")
	}
	w = get(s, "/export.csv?month=2026-10&provider=gcp&search=Database")
	rows, e := csv.NewReader(w.Body).ReadAll()
	if e != nil || len(rows) != 2 || rows[1][2] != "Database, managed" || rows[1][4] != "40.000000" {
		t.Fatalf("filtered export %v %v", rows, e)
	}
}
func TestFailedImportAndStalePreviewPreserveData(t *testing.T) {
	s, st, cookie := setup(t)
	before := st.Read()
	for _, text := range []string{strings.Replace(fixture, "2026-10-01", "2026-02-30", 1), strings.Replace(fixture, ",100,USD", ",NaN,USD", 1), strings.Replace(fixture, ",100,USD", ",100,EUR", 1)} {
		w := post(s, "/imports/preview", url.Values{"csv": {text}, "from": {"2026-10-01"}, "through": {"2026-10-08"}, "complete": {"yes"}}, cookie)
		if w.Code != 400 || st.Read().Active.ID != before.Active.ID {
			t.Fatal("invalid import changed snapshot")
		}
	}
	token := prepare(t, s, cookie, fixture)
	w := post(s, "/budget", url.Values{"month": {"2026-10"}, "amount": {"100"}}, cookie)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = post(s, "/imports/apply", url.Values{"token": {token}}, cookie)
	if w.Code != 409 || st.Read().Active.ID != before.Active.ID {
		t.Fatal("stale preview overwrote changed state")
	}
}
func TestSecurityBoundaries(t *testing.T) {
	s, st, cookie := setup(t)
	before := st.Read().Revision
	w := post(s, "/budget", url.Values{"month": {"2026-10"}, "amount": {"1"}}, nil)
	if w.Code != 403 || st.Read().Revision != before {
		t.Fatal("missing CSRF token admitted")
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/budget", strings.NewReader(url.Values{"csrf": {cookie.Value}, "month": {"2026-10"}, "amount": {"1"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://untrusted.example")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign origin admitted")
	}
	r = httptest.NewRequest("POST", "http://127.0.0.1:8080/budget", strings.NewReader(url.Values{"csrf": {cookie.Value}, "month": {"2026-10"}, "amount": {"10"}, "currency": {"USD"}, "snapshot": {st.Read().Active.ID}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "null")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal("opaque browser origin with valid CSRF rejected")
	}
	r = httptest.NewRequest("GET", "http://untrusted.example:8080/", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign host admitted")
	}
	w = get(s, "/")
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'none'") {
		t.Fatal("scripts not disabled by CSP")
	}
	if w = get(s, "/?month=not-a-month"); w.Code != 400 {
		t.Fatal("invalid filter silently ignored")
	}
}
func TestMultipartImportAndEscaping(t *testing.T) {
	s, st, cookie := setup(t)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for k, v := range map[string]string{"csrf": cookie.Value, "from": "2026-10-01", "through": "2026-10-08", "complete": "yes"} {
		form.WriteField(k, v)
	}
	file, e := form.CreateFormFile("file", "billing.csv")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(file, strings.Replace(fixture, "Compute", "<script>alert(1)</script>", -1))
	form.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/imports/preview", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal("uploaded labels not escaped", w.Code)
	}
	if !st.Read().Demo {
		t.Fatal("upload preview applied immediately")
	}
}
func TestBudgetAndAcknowledgment(t *testing.T) {
	s, st, cookie := setup(t)
	w := post(s, "/budget", url.Values{"month": {"2026-10"}, "amount": {"1000"}}, cookie)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	w = get(s, "/")
	if !strings.Contains(w.Body.String(), "412%") {
		t.Fatal("over-budget usage was clipped to 100 percent")
	}
	w = post(s, "/budget", url.Values{"month": {"2026-10"}, "amount": {"0"}}, cookie)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	w = get(s, "/")
	if !strings.Contains(w.Body.String(), "Over") {
		t.Fatal("zero budget did not report over limit")
	}
	key := regexp.MustCompile(`name="key" value="([a-f0-9]+)"`).FindStringSubmatch(w.Body.String())
	if len(key) != 2 {
		t.Fatal("demo missing alert form")
	}
	w = post(s, "/alerts", url.Values{"key": {key[1]}, "month": {"2026-10"}, "reviewed": {"yes"}}, cookie)
	if w.Code != 303 || !st.Read().Acknowledged[key[1]] {
		t.Fatal("review state not saved")
	}
	w = get(s, "/")
	if !strings.Contains(w.Body.String(), "Reviewed · Mark as open") {
		t.Fatal("review state not presented")
	}
}

func TestStaleBudgetDoesNotChangeCurrency(t *testing.T) {
	s, st, cookie := setup(t)
	old := st.Read().Active.ID
	token := prepare(t, s, cookie, strings.ReplaceAll(fixture, "USD", "EUR"))
	if w := post(s, "/imports/apply", url.Values{"token": {token}}, cookie); w.Code != 303 {
		t.Fatal(w.Code)
	}
	before := st.Read().Revision
	w := post(s, "/budget", url.Values{"month": {"2026-10"}, "amount": {"500"}, "currency": {"USD"}, "snapshot": {old}}, cookie)
	if w.Code != 409 || st.Read().Revision != before {
		t.Fatal("stale USD budget changed EUR workspace")
	}
}

func TestForecastOverBudgetIsVisible(t *testing.T) {
	s, _, cookie := setup(t)
	token := prepare(t, s, cookie, fixture)
	if w := post(s, "/imports/apply", url.Values{"token": {token}}, cookie); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if w := post(s, "/budget", url.Values{"month": {"2026-10"}, "amount": {"1000"}}, cookie); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if w := get(s, "/"); !strings.Contains(w.Body.String(), "Forecast above your budget") {
		t.Fatal("a projected overrun appeared healthy")
	}
}
