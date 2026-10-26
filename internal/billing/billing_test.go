package billing

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func fixedToday() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
func fixture(t *testing.T, name string, options ImportOptions) Preview {
	t.Helper()
	input, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	preview, err := ParseCSV(input, options)
	if err != nil {
		t.Fatal(err)
	}
	return preview
}
func options() ImportOptions {
	return ImportOptions{Name: "billing.csv", From: "2026-10-01", Through: "2026-10-08", Confirmed: true, Today: fixedToday()}
}
func workspace(s Snapshot) Workspace {
	return Workspace{Active: s, Budgets: map[string]Budget{}, Acknowledged: map[string]bool{}}
}
func money(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

func TestMoneyPrecisionAndLimits(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{{"0.10", "0.100000"}, {"-0.03", "-0.030000"}, {"+12.340001", "12.340001"}, {"1000000000", "1000000000.000000"}, {"-0", "0.000000"}} {
		t.Run(tc.input, func(t *testing.T) {
			value, err := ParseMoney(tc.input)
			if err != nil || Decimal(value) != tc.want {
				t.Fatalf("got %v, %v", value, err)
			}
		})
	}
	for _, input := range []string{"", "NaN", "Inf", "1e3", "1.0000001", ".1", "1.", "--1", "1000000000.000001", "9223372036854775807"} {
		if _, err := ParseMoney(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	if _, err := Add(MaxTotal, 1); err == nil {
		t.Fatal("positive overflow accepted")
	}
	if _, err := Add(-MaxTotal, -1); err == nil {
		t.Fatal("negative overflow accepted")
	}
	if got := Format(money("3889.285714"), "USD"); got != "$3,889.29" {
		t.Fatal(got)
	}
	if got := Format(money("-25"), "EUR"); got != "−€25.00" {
		t.Fatal(got)
	}
}

func TestKnownCostsForecastSpikesAndBudget(t *testing.T) {
	p := fixture(t, "known-costs.csv", options())
	if p.Net != money("965") || p.Gross != money("990") || p.Credits != money("-25") || len(p.Snapshot.Rows) != 10 {
		t.Fatalf("unexpected preview totals: %+v", p)
	}
	w := workspace(p.Snapshot)
	b := Budget{Month: "2026-10", Currency: "USD", Limit: money("4000")}
	w.Budgets[BudgetKey(b.Month, b.Provider, b.Currency)] = b
	a := Analyze(w, Query{Month: "2026-10"}, fixedToday())
	if a.Spend != p.Net || a.Gross != p.Gross || a.Credits != p.Credits || a.Remaining != money("3035") {
		t.Fatalf("unexpected analysis: %+v", a)
	}
	f := a.Forecast
	if !f.Available || f.Point != money("3889.285714") || f.SampleDays != 7 || f.RemainingDays != 23 || f.Through != "2026-10-08" {
		t.Fatalf("unexpected forecast: %+v", f)
	}
	if !(f.Low <= f.Point && f.Point <= f.High) {
		t.Fatalf("unordered estimate range: %+v", f)
	}
	if len(a.Spikes) != 2 || a.OpenSpikes != 2 {
		t.Fatalf("got %d spikes", len(a.Spikes))
	}
	for _, s := range a.Spikes {
		switch s.Provider {
		case "aws":
			if s.Baseline != money("100") || s.Actual != money("250") || s.Extra != money("150") || s.Percent != 150 || s.NewSpend {
				t.Fatalf("bad compute spike: %+v", s)
			}
		case "gcp":
			if !s.NewSpend || s.Baseline != 0 || s.Extra != money("40") {
				t.Fatalf("bad new spend: %+v", s)
			}
		}
		w.Acknowledged[s.Key] = true
	}
	if got := Analyze(w, Query{Month: "2026-10"}, fixedToday()); got.OpenSpikes != 0 {
		t.Fatal("acknowledgments not applied")
	}
	aws := Analyze(w, Query{Month: "2026-10", Provider: "aws"}, fixedToday())
	if aws.Spend != money("925") || aws.Budget != nil || len(aws.Spikes) != 1 {
		t.Fatalf("provider scope leaked: %+v", aws)
	}
	if rows := Filter(p.Snapshot, Query{Month: "2026-10", Search: "DATABASE, MANAGED"}); len(rows) != 1 || rows[0].Provider != "gcp" {
		t.Fatalf("search failed: %+v", rows)
	}
}

func TestDuplicateChargesAndCreditsArePreserved(t *testing.T) {
	p := fixture(t, "duplicate-credits.csv", options())
	if p.Net != money("0.17") || p.Gross != money("0.20") || p.Credits != money("-0.03") || len(p.Snapshot.Rows) != 3 {
		t.Fatalf("duplicate or credit lost: %+v", p)
	}
	rows := append([]Row(nil), p.Snapshot.Rows...)
	rows[0], rows[2] = rows[2], rows[0]
	s := p.Snapshot
	s.Rows = rows
	if Fingerprint(s) != p.Snapshot.ID {
		t.Fatal("row order changed dataset identity")
	}
	s.Rows = s.Rows[1:]
	if Fingerprint(s) == p.Snapshot.ID {
		t.Fatal("duplicate multiplicity absent from identity")
	}
}

func TestCoverageGatesForecastAndAlerts(t *testing.T) {
	base := fixture(t, "known-costs.csv", options()).Snapshot
	for _, tc := range []struct {
		name          string
		confirmed     bool
		from, through string
		today         time.Time
	}{{"unconfirmed", false, "2026-10-01", "2026-10-08", fixedToday()}, {"partial month", true, "2026-10-02", "2026-10-09", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}, {"less than seven days", true, "2026-10-01", "2026-10-06", fixedToday()}, {"current day excluded", true, "2026-10-01", "2026-10-08", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Coverage = Coverage{tc.from, tc.through, tc.confirmed}
			a := Analyze(workspace(s), Query{Month: "2026-10"}, tc.today)
			if a.Forecast.Available || a.Forecast.Reason == "" {
				t.Fatalf("unsafe forecast: %+v", a.Forecast)
			}
			if tc.name == "unconfirmed" && len(a.Spikes) != 0 {
				t.Fatal("unconfirmed alert baseline used")
			}
			if tc.name == "current day excluded" && a.Days[6].Complete {
				t.Fatal("current billing day treated as complete")
			}
		})
	}
	s := base
	s.Coverage.Through = "2026-10-31"
	a := Analyze(workspace(s), Query{Month: "2026-10"}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if !a.Forecast.FullMonth || a.Forecast.Point != money("965") || a.Forecast.RemainingDays != 0 {
		t.Fatalf("closed month projected: %+v", a.Forecast)
	}
}

func TestTinySpikeBaselineDoesNotOverflow(t *testing.T) {
	s := Snapshot{Currency: "USD", Coverage: Coverage{"2026-10-01", "2026-10-08", true}}
	for d := 1; d <= 7; d++ {
		s.Rows = append(s.Rows, Row{fmt.Sprintf("2026-10-%02d", d), "aws", "Compute", "USD", 1})
	}
	s.Rows = append(s.Rows, Row{"2026-10-08", "aws", "Compute", "USD", MaxRow})
	s.ID = Fingerprint(s)
	a := Analyze(workspace(s), Query{Month: "2026-10"}, fixedToday())
	if len(a.Spikes) != 1 || a.Spikes[0].Percent != 999999 || !a.Spikes[0].PercentCapped {
		t.Fatalf("overflowed percentage: %+v", a.Spikes)
	}
}

func TestCSVRejectsWholeInvalidDataset(t *testing.T) {
	for _, name := range []string{"invalid-date.csv", "mixed-currencies.csv", "invalid-amount.csv"} {
		t.Run(name, func(t *testing.T) {
			input, err := os.Open("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			p, err := ParseCSV(input, options())
			if err == nil || len(p.Snapshot.Rows) != 0 {
				t.Fatalf("partial invalid import: %+v %v", p, err)
			}
		})
	}
	for _, tc := range []struct {
		name, input string
		opts        ImportOptions
	}{{"future charge", "date,provider,service,currency,cost\n2026-10-10,aws,Compute,USD,1\n", ImportOptions{From: "2026-10-01", Through: "2026-10-10", Today: fixedToday()}}, {"duplicate header", "date,provider,service,cost,cost\n2026-10-01,aws,Compute,1,1", options()}, {"too large", strings.Repeat("x", MaxBytes+1), options()}, {"bad precision", "date,provider,service,currency,cost\n2026-10-01,aws,Compute,USD,0.0000001", options()}, {"invalid leap day", "date,provider,service,currency,cost\n2026-02-29,aws,Compute,USD,1", options()}} {
		t.Run(tc.name, func(t *testing.T) {
			if p, err := ParseCSV(strings.NewReader(tc.input), tc.opts); err == nil || len(p.Snapshot.Rows) != 0 {
				t.Fatalf("invalid import accepted: %+v %v", p, err)
			}
		})
	}
}

func TestCSVQuotedNewlinesAndExport(t *testing.T) {
	input := "\ufeffdate,provider,service,currency,cost\n2026-10-01,aws,\"Compute,\nreserved\",USD,0.100001\n2026-10-01,gcp,=SUM(A1),USD,-0.03\n"
	p, err := ParseCSV(strings.NewReader(input), options())
	if err != nil {
		t.Fatal(err)
	}
	if p.Snapshot.Rows[0].Service != "Compute,\nreserved" {
		t.Fatal("quoted newline lost")
	}
	var output bytes.Buffer
	if err = ExportCSV(&output, p.Snapshot.Rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "'=SUM(A1)") || !strings.Contains(output.String(), "0.100001") {
		t.Fatal(output.String())
	}
	reread, err := ParseCSV(strings.NewReader(output.String()), options())
	if err != nil || reread.Net != p.Net {
		t.Fatalf("export changed money: %+v %v", reread, err)
	}
}

func TestReplaceClearsDatasetScopedAcknowledgments(t *testing.T) {
	p := fixture(t, "known-costs.csv", options())
	w := workspace(p.Snapshot)
	key := AlertKey(w.Active.ID, "aws", "Compute", "2026-10-08")
	w.Acknowledged[key] = true
	w.Demo = true
	same := Replace(w, p.Snapshot)
	if !same.Acknowledged[key] {
		t.Fatal("identical retry cleared acknowledgment")
	}
	changed := fixture(t, "duplicate-credits.csv", options()).Snapshot
	next := Replace(w, changed)
	if next.Demo || len(next.Acknowledged) != 0 || next.Active.ID != changed.ID {
		t.Fatalf("replacement retained dataset state: %+v", next)
	}
}

func TestCSVRowAndAggregateCaps(t *testing.T) {
	header := "date,provider,service,currency,cost\n"
	row := "2026-10-01,aws,Compute,USD,0.000001\n"
	valid := header + strings.Repeat(row, MaxRows)
	p, err := ParseCSV(strings.NewReader(valid), options())
	if err != nil || len(p.Snapshot.Rows) != MaxRows {
		t.Fatalf("exact row cap rejected: %d %v", len(p.Snapshot.Rows), err)
	}
	if _, err = ParseCSV(strings.NewReader(valid+row), options()); err == nil {
		t.Fatal("row cap exceeded")
	}
	costly := "2026-10-01,aws,Compute,USD,1000000000\n"
	if _, err = ParseCSV(strings.NewReader(header+strings.Repeat(costly, 101)), options()); err == nil {
		t.Fatal("aggregate cap exceeded")
	}
}

func TestSavedWorkspaceValidationRejectsInvalidMaps(t *testing.T) {
	base := fixture(t, "known-costs.csv", options()).Snapshot
	for _, change := range []func(*Workspace){func(w *Workspace) { w.Budgets["wrong"] = Budget{Month: "2026-10", Currency: "USD", Limit: 1} }, func(w *Workspace) {
		w.Budgets[BudgetKey("2026-10", "", "USD")] = Budget{Month: "2026-10", Currency: "USD", Limit: -1}
	}, func(w *Workspace) { w.Acknowledged["bad"] = true }, func(w *Workspace) { w.Acknowledged[strings.Repeat("a", 64)] = false }, func(w *Workspace) { w.Active.Rows = append([]Row(nil), w.Active.Rows...); w.Active.Rows[0].Cost++ }} {
		w := workspace(base)
		change(&w)
		if err := ValidateWorkspace(w); err == nil {
			t.Fatal("invalid persisted workspace accepted")
		}
	}
}

func TestIdenticalImportRefreshesProvenance(t *testing.T) {
	w := Demo(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	key := AlertKey(w.Active.ID, "gcp", "BigQuery · Analytics", "2026-10-18")
	w.Acknowledged[key] = true
	imported := w.Active
	imported.Name = "My actual billing.csv"
	imported.ImportedAt = imported.ImportedAt.Add(time.Hour)
	next := Replace(w, imported)
	if next.Demo || next.Active.Name != imported.Name || !next.Active.ImportedAt.Equal(imported.ImportedAt) || !next.Acknowledged[key] {
		t.Fatal("identical billing import did not update provenance while preserving review state")
	}
}
