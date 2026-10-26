package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Koquda/VaultPilot/internal/billing"
	"github.com/Koquda/VaultPilot/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type pending struct {
	Snapshot billing.Snapshot
	Revision uint64
	Expires  time.Time
}
type Server struct {
	store     *store.Store
	templates *template.Template
	csrf      string
	port      string
	mu        sync.Mutex
	previews  map[string]pending
	now       func() time.Time
}
type option struct {
	Value, Label string
	Selected     bool
}
type serviceView struct{ Name, Provider, Total, Gross, Width, Color string }
type spikeView struct {
	Key, Date, Provider, Service, Actual, Baseline, Extra, Change string
	Acknowledged                                                  bool
}
type chart struct {
	Actual, Forecast, Area string
	Ticks                  []string
	Labels                 []string
	HasActual, HasForecast bool
	ZeroY                  string
}
type page struct {
	Title, Active, CSRF, Error, Notice, Month, Provider, Search, Currency                                string
	Source, CoverageFrom, CoverageThrough, LastCharge, ImportedAt                                        string
	Demo, Confirmed                                                                                      bool
	Months, Providers                                                                                    []option
	A                                                                                                    billing.Analysis
	Spend, Gross, Credits, Forecast, Budget, Remaining, ForecastGap, RunRate, BudgetInput, Percent, Ring string
	BudgetSet, HasForecast, OverBudget, AtRisk                                                           bool
	Services                                                                                             []serviceView
	Spikes                                                                                               []spikeView
	Chart                                                                                                chart
	Rows                                                                                                 []billing.Row
	Token, SnapshotID                                                                                    string
	Preview                                                                                              *billing.Preview
	PreviewNet, PreviewGross, PreviewCredits                                                             string
	FormFrom, FormThrough, FormCSV                                                                       string
}

func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func New(st *store.Store, address string) (*Server, error) {
	_, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	funcs := template.FuncMap{"money": billing.Format, "date": billing.ShortDate, "provider": billing.ProviderName, "decimal": billing.Decimal, "monthLabel": billing.MonthLabel, "add": func(a, b int) int { return a + b }, "icon": icon}
	t, e := template.New("layout").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if e != nil {
		return nil, e
	}
	return &Server{store: st, templates: t, csrf: randomToken(), port: port, previews: map[string]pending{}, now: time.Now}, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, e := net.SplitHostPort(r.Host)
	if e != nil || port != s.port || (host != "127.0.0.1" && host != "localhost") {
		http.Error(w, "Open VaultPilot at its localhost address.", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self'; script-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		http.SetCookie(w, &http.Cookie{Name: "vaultpilot_csrf", Value: s.csrf, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		switch r.URL.Path {
		case "/", "/transactions", "/imports", "/settings":
			if r.URL.Path != "/imports" {
				if _, err := parseQuery(r.URL.Query(), s.store.Read().Active); err != nil {
					s.render(w, r, 400, err.Error())
					return
				}
			}
			s.render(w, r, 200, "")
		case "/static/style.css":
			b, _ := assets.ReadFile("static/style.css")
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Write(b)
		case "/sample.csv":
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="vaultpilot-sample.csv"`)
			io.WriteString(w, billing.SampleCSV())
		case "/export.csv":
			state := s.store.Read()
			q, e := parseQuery(r.URL.Query(), state.Active)
			if e != nil {
				http.Error(w, e.Error(), 400)
				return
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="vaultpilot-costs.csv"`)
			billing.ExportCSV(w, billing.Filter(state.Active, q))
		default:
			http.NotFound(w, r)
		}
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", 405)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && origin != "http://"+r.Host {
		http.Error(w, "Request origin is not allowed.", 403)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, billing.MaxBytes+65536)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if e := r.ParseMultipartForm(billing.MaxBytes + 32768); e != nil {
			s.render(w, r, 400, "The upload is too large or could not be read.")
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
	} else if e := r.ParseForm(); e != nil {
		s.render(w, r, 400, "The form could not be read.")
		return
	}
	cookie, e := r.Cookie("vaultpilot_csrf")
	token := r.PostFormValue("csrf")
	if e != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.csrf)) != 1 || subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) != 1 {
		http.Error(w, "This form has expired. Reload the page and try again.", 403)
		return
	}
	switch r.URL.Path {
	case "/imports/preview":
		s.preview(w, r)
	case "/imports/apply":
		s.apply(w, r)
	case "/budget":
		s.budget(w, r)
	case "/alerts":
		s.alert(w, r)
	case "/reset":
		s.reset(w, r)
	default:
		http.NotFound(w, r)
	}
}
func parseQuery(v url.Values, s billing.Snapshot) (billing.Query, error) {
	months := billing.Months(s)
	q := billing.Query{Month: v.Get("month"), Provider: v.Get("provider"), Search: strings.TrimSpace(v.Get("search"))}
	if q.Month == "" {
		q.Month = months[0]
	}
	valid := false
	for _, m := range months {
		if m == q.Month {
			valid = true
		}
	}
	if !valid {
		return q, fmt.Errorf("Select a month covered by this dataset.")
	}
	if q.Provider != "" && !billing.ValidProvider(q.Provider) {
		return q, fmt.Errorf("Select a supported cloud provider.")
	}
	if len(q.Search) > 200 {
		return q, fmt.Errorf("Search must contain at most 200 characters.")
	}
	return q, nil
}
func (s *Server) model(r *http.Request) page {
	state := s.store.Read()
	q, e := parseQuery(r.URL.Query(), state.Active)
	if e != nil {
		q, _ = parseQuery(url.Values{}, state.Active)
	}
	a := billing.Analyze(state, q, s.now().UTC())
	p := page{Title: "Overview", Active: "overview", CSRF: s.csrf, Month: q.Month, Provider: q.Provider, Search: q.Search, Currency: a.Currency, A: a, Source: state.Active.Name, Demo: state.Demo, Confirmed: state.Active.Coverage.Confirmed, CoverageFrom: billing.DateLabel(state.Active.Coverage.From), CoverageThrough: billing.DateLabel(state.Active.Coverage.Through), LastCharge: billing.DateLabel(a.LastCharge), ImportedAt: state.Active.ImportedAt.Format("Jan 2, 2006"), Spend: billing.Format(a.Spend, a.Currency), Gross: billing.Format(a.Gross, a.Currency), Credits: billing.Format(a.Credits, a.Currency), Rows: a.Rows, Forecast: "Unavailable", Budget: "Not set", Remaining: "Not set", BudgetInput: "8000", Percent: "0"}
	switch r.URL.Path {
	case "/transactions":
		p.Title = "Transactions"
		p.Active = "transactions"
	case "/imports", "/imports/preview", "/imports/apply":
		p.Title = "Imports"
		p.Active = "imports"
	case "/settings", "/reset":
		p.Title = "Settings"
		p.Active = "settings"
	}
	p.SnapshotID = state.Active.ID
	p.Ring = "0"
	for _, m := range billing.Months(state.Active) {
		p.Months = append(p.Months, option{m, billing.MonthLabel(m), m == q.Month})
	}
	p.Providers = append(p.Providers, option{"", "All providers", q.Provider == ""})
	for _, v := range billing.Providers(state.Active) {
		p.Providers = append(p.Providers, option{v, billing.ProviderName(v), v == q.Provider})
	}
	p.HasForecast = a.Forecast.Available
	if p.HasForecast {
		p.Forecast = billing.Format(a.Forecast.Point, a.Currency)
		p.RunRate = billing.Format(a.Forecast.RunRate, a.Currency)
	}
	if a.Budget != nil {
		p.BudgetSet = true
		p.Budget = billing.Format(a.Budget.Limit, a.Currency)
		p.BudgetInput = strings.TrimRight(strings.TrimRight(billing.Decimal(a.Budget.Limit), "0"), ".")
		p.Remaining = billing.Format(a.Remaining, a.Currency)
		ratio := 0.0
		if a.Budget.Limit > 0 {
			ratio = float64(a.Spend) / float64(a.Budget.Limit) * 100
		} else if a.Spend > 0 {
			ratio = 100
		}
		p.Ring = strconv.FormatFloat(min(100, max(0, ratio)), 'f', 0, 64)
		p.Percent = strconv.FormatFloat(max(0, ratio), 'f', 0, 64)
		if a.Budget.Limit == 0 && a.Spend > 0 {
			p.Percent = "Over"
		} else if ratio > 999999 {
			p.Percent = "999999+"
		}
		p.OverBudget = a.Spend > a.Budget.Limit
		p.AtRisk = p.HasForecast && a.Forecast.Point > a.Budget.Limit
		if p.HasForecast {
			p.ForecastGap = billing.Format(a.Budget.Limit-a.Forecast.Point, a.Currency)
		}
	}
	maxService := billing.Money(1)
	for _, v := range a.Services {
		if v.Gross > maxService {
			maxService = v.Gross
		}
	}
	colors := []string{"#3485e4", "#299194", "#9980d4", "#c28625", "#7791ad", "#9a7482"}
	for i, v := range a.Services {
		p.Services = append(p.Services, serviceView{v.Name, billing.ProviderName(v.Provider), billing.Format(v.Net, a.Currency), billing.Format(v.Gross, a.Currency), fmt.Sprintf("%.2f", float64(v.Gross)/float64(maxService)*100), colors[i%len(colors)]})
	}
	for _, v := range a.Spikes {
		change := fmt.Sprintf("+%d%%", v.Percent)
		if v.PercentCapped {
			change = ">999999%"
		}
		if v.NewSpend {
			change = "New spend"
		}
		p.Spikes = append(p.Spikes, spikeView{v.Key, billing.DateLabel(v.Date), billing.ProviderName(v.Provider), v.Service, billing.Format(v.Actual, a.Currency), billing.Format(v.Baseline, a.Currency), billing.Format(v.Extra, a.Currency), change, v.Acknowledged})
	}
	p.Chart = makeChart(a)
	month, _ := billing.Month(q.Month)
	p.FormFrom = month.Format("2006-01-02")
	p.FormThrough = state.Active.Coverage.Through
	switch r.URL.Query().Get("notice") {
	case "imported":
		p.Notice = "Billing snapshot imported. Your dashboard now uses the new data."
	case "budget":
		p.Notice = "Budget saved for this month and provider selection."
	case "alert":
		p.Notice = "Spike review status saved."
	case "reset":
		p.Notice = "Demo workspace restored."
	}
	return p
}
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, message string) {
	p := s.model(r)
	p.Error = message
	s.write(w, status, p)
}
func (s *Server) write(w http.ResponseWriter, status int, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if e := s.templates.ExecuteTemplate(w, "layout", p); e != nil {
		fmt.Printf("template: %v\n", e)
	}
}
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var input io.Reader = strings.NewReader(r.PostFormValue("csv"))
	name := "Pasted billing CSV"
	if r.MultipartForm != nil {
		f, h, err := r.FormFile("file")
		if err == nil {
			defer f.Close()
			input = f
			name = h.Filename
		} else if err != http.ErrMissingFile {
			s.render(w, r, 400, "The billing file could not be read.")
			return
		}
	}
	preview, e := billing.ParseCSV(input, billing.ImportOptions{Name: name, From: r.PostFormValue("from"), Through: r.PostFormValue("through"), Confirmed: r.PostFormValue("complete") == "yes", Today: s.now().UTC()})
	if e != nil {
		p := s.model(r)
		p.Error = e.Error()
		p.FormFrom = r.PostFormValue("from")
		p.FormThrough = r.PostFormValue("through")
		p.FormCSV = r.PostFormValue("csv")
		s.write(w, 400, p)
		return
	}
	state := s.store.Read()
	token := randomToken()
	s.mu.Lock()
	for k, v := range s.previews {
		if s.now().After(v.Expires) {
			delete(s.previews, k)
		}
	}
	if len(s.previews) >= 16 {
		s.mu.Unlock()
		s.render(w, r, 429, "Too many pending previews. Wait 15 minutes before trying again.")
		return
	}
	s.previews[token] = pending{preview.Snapshot, state.Revision, s.now().Add(15 * time.Minute)}
	s.mu.Unlock()
	p := s.model(r)
	p.Token = token
	p.Preview = &preview
	p.PreviewNet = billing.Format(preview.Net, preview.Snapshot.Currency)
	p.PreviewGross = billing.Format(preview.Gross, preview.Snapshot.Currency)
	p.PreviewCredits = billing.Format(preview.Credits, preview.Snapshot.Currency)
	s.write(w, 200, p)
}
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.previews[token]
	if !ok || s.now().After(pending.Expires) {
		s.render(w, r, 409, "This preview has expired. Preview the file again.")
		return
	}
	e := s.store.Update(func(w *billing.Workspace) error {
		if w.Revision != pending.Revision && w.Active.ID != pending.Snapshot.ID {
			return fmt.Errorf("Your workspace changed after this preview. Preview the file again before replacing it.")
		}
		*w = billing.Replace(*w, pending.Snapshot)
		return nil
	})
	if e != nil {
		s.render(w, r, 409, e.Error())
		return
	}
	delete(s.previews, token)
	http.Redirect(w, r, "/?notice=imported", 303)
}
func (s *Server) budget(w http.ResponseWriter, r *http.Request) {
	m := r.PostFormValue("month")
	provider := r.PostFormValue("provider")
	currency := r.PostFormValue("currency")
	snapshot := r.PostFormValue("snapshot")
	if _, e := billing.Month(m); e != nil || provider != "" && !billing.ValidProvider(provider) {
		s.render(w, r, 400, "Select a valid month and provider.")
		return
	}
	amount, e := billing.ParseMoney(r.PostFormValue("amount"))
	if e != nil || amount < 0 {
		s.render(w, r, 400, "Budget must be a nonnegative decimal amount within the supported limit.")
		return
	}
	stale := false
	e = s.store.Update(func(w *billing.Workspace) error {
		if w.Active.ID != snapshot || w.Active.Currency != currency {
			stale = true
			return fmt.Errorf("Billing data changed after this form was opened. Reload Settings before editing its budget.")
		}
		b := billing.Budget{Month: m, Provider: provider, Currency: currency, Limit: amount}
		w.Budgets[billing.BudgetKey(m, provider, currency)] = b
		return nil
	})
	if e != nil {
		status := 500
		if stale {
			status = 409
		}
		s.render(w, r, status, "Budget was not saved. "+e.Error())
		return
	}
	http.Redirect(w, r, "/settings?"+url.Values{"month": {m}, "provider": {provider}, "notice": {"budget"}}.Encode(), 303)
}
func (s *Server) alert(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	m := r.PostFormValue("month")
	provider := r.PostFormValue("provider")
	e := s.store.Update(func(w *billing.Workspace) error {
		q, e := parseQuery(url.Values{"month": {m}, "provider": {provider}}, w.Active)
		if e != nil {
			return e
		}
		a := billing.Analyze(*w, q, s.now().UTC())
		for _, spike := range a.Spikes {
			if spike.Key == key {
				if r.PostFormValue("reviewed") == "yes" {
					w.Acknowledged[key] = true
				} else {
					delete(w.Acknowledged, key)
				}
				return nil
			}
		}
		return fmt.Errorf("This spike no longer belongs to the active dataset.")
	})
	if e != nil {
		s.render(w, r, 400, e.Error())
		return
	}
	http.Redirect(w, r, "/?"+url.Values{"month": {m}, "provider": {provider}, "notice": {"alert"}}.Encode(), 303)
}
func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "yes" {
		s.render(w, r, 400, "Confirm that you want to replace the current workspace with demo data.")
		return
	}
	e := s.store.Update(func(w *billing.Workspace) error { *w = billing.Demo(s.now().UTC()); return nil })
	if e != nil {
		s.render(w, r, 500, "The workspace was not reset. "+e.Error())
		return
	}
	http.Redirect(w, r, "/?notice=reset", 303)
}
func makeChart(a billing.Analysis) chart {
	c := chart{Labels: []string{"01", "07", "14", "21", fmt.Sprintf("%02d", len(a.Days))}}
	maxValue := billing.Money(1)
	minValue := billing.Money(0)
	for _, d := range a.Days {
		if d.Net > maxValue {
			maxValue = d.Net
		}
		if d.Net < minValue {
			minValue = d.Net
		}
		if d.Projected > maxValue {
			maxValue = d.Projected
		}
	}
	span := float64(maxValue - minValue)
	y := func(m billing.Money) float64 { return 28 + (float64(maxValue-m)/span)*190 }
	x := func(i int) float64 { return 48 + float64(i)/float64(max(1, len(a.Days)-1))*642 }
	c.ZeroY = fmt.Sprintf("%.1f", y(0))
	actual := []string{}
	projected := []string{}
	last := ""
	for i, d := range a.Days {
		pt := fmt.Sprintf("%.1f,%.1f", x(i), y(d.Net))
		if d.Date >= a.Forecast.Through && a.Forecast.Available && !a.Forecast.FullMonth {
			if d.Date == a.Forecast.Through {
				projected = append(projected, pt)
			} else {
				projected = append(projected, fmt.Sprintf("%.1f,%.1f", x(i), y(d.Projected)))
			}
		}
		if d.Date >= a.Days[0].Date && d.Date <= a.Forecast.Through && d.Date >= a.Query.Month+"-01" {
			if d.Complete || d.Net != 0 {
				actual = append(actual, pt)
				last = pt
			}
		}
	}
	if !a.Forecast.Available {
		actual = nil
		for i, d := range a.Days {
			if d.Net != 0 || d.Complete {
				actual = append(actual, fmt.Sprintf("%.1f,%.1f", x(i), y(d.Net)))
				last = actual[len(actual)-1]
			}
		}
	}
	c.Actual = strings.Join(actual, " ")
	c.Forecast = strings.Join(projected, " ")
	c.HasActual = len(actual) > 0
	c.HasForecast = len(projected) > 1
	if c.HasActual {
		firstX := strings.Split(actual[0], ",")[0]
		lastX := strings.Split(last, ",")[0]
		c.Area = firstX + "," + c.ZeroY + " " + c.Actual + " " + lastX + "," + c.ZeroY
	}
	for i := 0; i < 4; i++ {
		value := maxValue - billing.Money(float64(maxValue-minValue)*float64(i)/3)
		c.Ticks = append(c.Ticks, billing.Format(value, a.Currency))
	}
	return c
}
func icon(name string) template.HTML {
	paths := map[string]string{"grid": `<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>`, "rows": `<path d="M8 5h13M8 12h13M8 19h13M3 5h.01M3 12h.01M3 19h.01"/>`, "import": `<path d="M12 3v12m-4-4 4 4 4-4M4 15v5a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-5"/>`, "settings": `<path d="M4 7h16M4 17h16"/><circle cx="9" cy="7" r="3"/><circle cx="15" cy="17" r="3"/>`, "arrow": `<path d="M5 12h14m-5-5 5 5-5 5"/>`, "shield": `<path d="m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Z"/><path d="m8 12 3 3 5-6"/>`, "trend": `<path d="m3 17 6-6 4 4 8-10m-6 0h6v6"/>`, "wallet": `<rect x="3" y="5" width="18" height="15" rx="3"/><path d="M3 8h18m-5 5h5"/>`, "alert": `<path d="m12 3 10 18H2L12 3Z"/><path d="M12 9v5m0 3h.01"/>`, "cloud": `<path d="M6 19a5 5 0 0 1-1-10 7 7 0 0 1 13-2 6 6 0 0 1 0 12Z"/>`, "vault": `<rect x="3" y="3" width="18" height="18" rx="4"/><circle cx="12" cy="12" r="4"/><path d="m9 9 6 6m0-6-6 6M6 8v8"/>`}
	path, ok := paths[name]
	if !ok {
		return ""
	}
	return template.HTML(`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + path + `</svg>`)
}
