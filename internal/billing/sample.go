package billing

import (
	"fmt"
	"time"
)

func Demo(today time.Time) Workspace {
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	if today.Day() < 22 {
		month = month.AddDate(0, -1, 0)
	}
	s := Snapshot{Name: "Northstar demo billing.csv", Currency: "USD", ImportedAt: today.UTC(), Coverage: Coverage{month.Format("2006-01-02"), month.AddDate(0, 0, 19).Format("2006-01-02"), true}}
	items := []struct {
		p, n string
		cost int64
	}{{"aws", "EC2 · Compute", 74000000}, {"aws", "RDS · Database", 42000000}, {"gcp", "BigQuery · Analytics", 33000000}, {"aws", "S3 · Storage", 18000000}, {"azure", "App Service", 27000000}, {"gcp", "Cloud Storage", 12000000}}
	for d := 0; d < 20; d++ {
		for i, item := range items {
			cost := item.cost + int64((d*7+i*3)%11-5)*800000
			if d == 17 && i == 2 {
				cost = 164000000
			}
			s.Rows = append(s.Rows, Row{month.AddDate(0, 0, d).Format("2006-01-02"), item.p, item.n, "USD", Money(cost)})
		}
	}
	s.Rows = append(s.Rows, Row{month.AddDate(0, 0, 14).Format("2006-01-02"), "aws", "Service credit", "USD", -125000000})
	s.ID = Fingerprint(s)
	m := month.Format("2006-01")
	budgets := map[string]Budget{}
	for _, b := range []Budget{{m, "", "USD", 8000000000}, {m, "aws", "USD", 4800000000}, {m, "gcp", "USD", 2200000000}, {m, "azure", "USD", 1100000000}} {
		budgets[BudgetKey(b.Month, b.Provider, b.Currency)] = b
	}
	return Workspace{Active: s, Budgets: budgets, Acknowledged: map[string]bool{}, Demo: true}
}
func SampleCSV() string {
	return "date,provider,service,currency,cost\n2026-10-01,aws,EC2 Compute,USD,74.250000\n2026-10-01,gcp,BigQuery,USD,32.500000\n2026-10-02,azure,App Service,USD,26.000000\n2026-10-02,aws,Service credit,USD,-5.000000\n"
}
func MonthLabel(value string) string {
	t, e := Month(value)
	if e != nil {
		return value
	}
	return t.Format("January 2006")
}
func DateLabel(value string) string {
	t, e := Day(value)
	if e != nil {
		return "No charges"
	}
	return t.Format("Jan 2, 2006")
}
func ShortDate(value string) string {
	t, e := Day(value)
	if e != nil {
		return value
	}
	return t.Format("Jan 2")
}
func Count(n int) string { return fmt.Sprintf("%d", n) }
