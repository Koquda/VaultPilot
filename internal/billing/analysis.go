package billing

import (
	"math/big"
	"sort"
	"strings"
	"time"
)

type Query struct {
	Month    string
	Provider string
	Search   string
}
type Daily struct {
	Date      string
	Net       Money
	Gross     Money
	Complete  bool
	Projected Money
}
type Service struct {
	Provider string
	Name     string
	Net      Money
	Gross    Money
}
type Spike struct {
	Key           string
	Date          string
	Provider      string
	Service       string
	Actual        Money
	Baseline      Money
	Extra         Money
	Percent       int
	PercentCapped bool
	NewSpend      bool
	Acknowledged  bool
	BaselineDays  int
}
type Forecast struct {
	Available     bool
	Reason        string
	Through       string
	Point         Money
	Low           Money
	High          Money
	Actual        Money
	SampleDays    int
	RemainingDays int
	RunRate       Money
	FullMonth     bool
}
type Analysis struct {
	Query      Query
	Currency   string
	Spend      Money
	Gross      Money
	Credits    Money
	Rows       []Row
	Days       []Daily
	Services   []Service
	Spikes     []Spike
	Forecast   Forecast
	Budget     *Budget
	Remaining  Money
	OpenSpikes int
	LastCharge string
}

func Months(s Snapshot) []string {
	set := map[string]bool{}
	from, _ := Day(s.Coverage.From)
	through, _ := Day(s.Coverage.Through)
	for d := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !d.After(through); d = d.AddDate(0, 1, 0) {
		set[d.Format("2006-01")] = true
	}
	months := []string{}
	for m := range set {
		months = append(months, m)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	return months
}
func Providers(s Snapshot) []string {
	set := map[string]bool{}
	for _, r := range s.Rows {
		set[r.Provider] = true
	}
	p := []string{}
	for v := range set {
		p = append(p, v)
	}
	sort.Strings(p)
	return p
}
func Filter(s Snapshot, q Query) []Row {
	rows := []Row{}
	search := strings.ToLower(strings.TrimSpace(q.Search))
	for _, r := range s.Rows {
		if strings.HasPrefix(r.Date, q.Month) && (q.Provider == "" || q.Provider == r.Provider) && (search == "" || strings.Contains(strings.ToLower(r.Service+" "+ProviderName(r.Provider)+" "+r.Date), search)) {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Date > rows[j].Date })
	return rows
}
func Analyze(w Workspace, q Query, today time.Time) Analysis {
	s := w.Active
	a := Analysis{Query: q, Currency: s.Currency, Rows: Filter(s, q), Services: []Service{}, Spikes: []Spike{}}
	start, _ := Month(q.Month)
	end := start.AddDate(0, 1, 0).AddDate(0, 0, -1)
	closed := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1).Format("2006-01-02")
	cutoff := s.Coverage.Through
	if cutoff > closed {
		cutoff = closed
	}
	if cutoff > end.Format("2006-01-02") {
		cutoff = end.Format("2006-01-02")
	}
	daily := map[string]*Daily{}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		daily[day] = &Daily{Date: day, Complete: s.Coverage.Confirmed && day >= s.Coverage.From && day <= cutoff}
	}
	services := map[string]*Service{}
	grossSeries := map[string]map[string]Money{}
	serviceNames := map[string]Row{}
	for _, r := range s.Rows {
		if q.Provider != "" && q.Provider != r.Provider {
			continue
		}
		key := r.Provider + "\x00" + r.Service
		if grossSeries[key] == nil {
			grossSeries[key] = map[string]Money{}
			serviceNames[key] = r
		}
		if r.Cost > 0 {
			grossSeries[key][r.Date] += r.Cost
		}
		if !strings.HasPrefix(r.Date, q.Month) {
			continue
		}
		a.Spend += r.Cost
		if r.Cost > 0 {
			a.Gross += r.Cost
		} else {
			a.Credits += r.Cost
		}
		if r.Date > a.LastCharge {
			a.LastCharge = r.Date
		}
		day := daily[r.Date]
		day.Net += r.Cost
		if r.Cost > 0 {
			day.Gross += r.Cost
		}
		if services[key] == nil {
			services[key] = &Service{Provider: r.Provider, Name: r.Service}
		}
		services[key].Net += r.Cost
		if r.Cost > 0 {
			services[key].Gross += r.Cost
		}
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		a.Days = append(a.Days, *daily[d.Format("2006-01-02")])
	}
	for _, v := range services {
		a.Services = append(a.Services, *v)
	}
	sort.Slice(a.Services, func(i, j int) bool { return a.Services[i].Net > a.Services[j].Net })
	for key, series := range grossSeries {
		r := serviceNames[key]
		for _, day := range a.Days {
			candidate, _ := Day(day.Date)
			prior := candidate.AddDate(0, 0, -7)
			if !day.Complete || prior.Format("2006-01-02") < s.Coverage.From {
				continue
			}
			var sum Money
			for i := 1; i <= 7; i++ {
				sum += series[candidate.AddDate(0, 0, -i).Format("2006-01-02")]
			}
			base := Ratio(sum, 1, 7)
			actual := series[day.Date]
			extra := actual - base
			if actual <= Ratio(base, 3, 2) || extra < 20*Money(Scale) {
				continue
			}
			percent, capped := spikePercent(extra, base)
			spike := Spike{Key: AlertKey(s.ID, r.Provider, r.Service, day.Date), Date: day.Date, Provider: r.Provider, Service: r.Service, Actual: actual, Baseline: base, Extra: extra, Percent: percent, PercentCapped: capped, NewSpend: base == 0, BaselineDays: 7}
			spike.Acknowledged = w.Acknowledged[spike.Key]
			if !spike.Acknowledged {
				a.OpenSpikes++
			}
			a.Spikes = append(a.Spikes, spike)
		}
	}
	sort.Slice(a.Spikes, func(i, j int) bool {
		if a.Spikes[i].Date == a.Spikes[j].Date {
			return a.Spikes[i].Extra > a.Spikes[j].Extra
		}
		return a.Spikes[i].Date > a.Spikes[j].Date
	})
	a.Forecast = forecast(s, a.Days, start, end, cutoff)
	if a.Forecast.Available {
		for i := range a.Days {
			if a.Days[i].Date > cutoff {
				a.Days[i].Projected = a.Forecast.RunRate
			}
		}
	}
	if b, ok := w.Budgets[BudgetKey(q.Month, q.Provider, s.Currency)]; ok {
		a.Budget = &b
		a.Remaining = b.Limit - a.Spend
	}
	return a
}
func forecast(s Snapshot, days []Daily, start, end time.Time, cutoff string) Forecast {
	f := Forecast{Through: cutoff}
	if !s.Coverage.Confirmed {
		f.Reason = "Confirm complete coverage to enable a forecast."
		return f
	}
	if s.Coverage.From > start.Format("2006-01-02") {
		f.Reason = "Coverage must begin on or before the first of this month."
		return f
	}
	if cutoff < start.Format("2006-01-02") {
		f.Reason = "This month has no completed covered billing days."
		return f
	}
	var covered []Daily
	for _, d := range days {
		if d.Date <= cutoff {
			covered = append(covered, d)
			f.Actual += d.Net
		}
	}
	if cutoff == end.Format("2006-01-02") {
		f.Available = true
		f.FullMonth = true
		f.Point = f.Actual
		f.Low = f.Actual
		f.High = f.Actual
		f.SampleDays = len(covered)
		return f
	}
	if len(covered) < 7 {
		f.Reason = "At least seven complete days from month start are required."
		return f
	}
	recent := covered[len(covered)-7:]
	var gross Money
	values := []Money{}
	for _, d := range recent {
		gross += d.Gross
		values = append(values, d.Gross)
	}
	f.SampleDays = 7
	f.RemainingDays = len(days) - len(covered)
	f.RunRate = Ratio(gross, 1, 7)
	f.Point = f.Actual + Ratio(gross, int64(f.RemainingDays), 7)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	f.Low = f.Actual + values[1]*Money(f.RemainingDays)
	f.High = f.Actual + values[5]*Money(f.RemainingDays)
	if f.Low > f.Point {
		f.Low = f.Point
	}
	if f.High < f.Point {
		f.High = f.Point
	}
	f.Available = true
	return f
}

func spikePercent(extra, baseline Money) (int, bool) {
	if baseline <= 0 {
		return 0, false
	}
	value := new(big.Int).Mul(big.NewInt(int64(extra)), big.NewInt(100))
	value.Add(value, big.NewInt(int64(baseline)/2))
	value.Quo(value, big.NewInt(int64(baseline)))
	const maximum = 999999
	if value.Cmp(big.NewInt(maximum)) > 0 {
		return maximum, true
	}
	return int(value.Int64()), false
}
