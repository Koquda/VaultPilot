package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Money int64

const Scale int64 = 1000000
const MaxTotal Money = 100000000000000000
const MaxRow Money = 1000000000000000
const MaxRows = 25000
const MaxBytes = 2 << 20

type Row struct {
	Date     string
	Provider string
	Service  string
	Currency string
	Cost     Money
}
type Coverage struct {
	From      string
	Through   string
	Confirmed bool
}
type Snapshot struct {
	ID         string
	Name       string
	ImportedAt time.Time
	Currency   string
	Rows       []Row
	Coverage   Coverage
}
type Budget struct {
	Month    string
	Provider string
	Currency string
	Limit    Money
}
type Workspace struct {
	Revision     uint64
	Active       Snapshot
	Budgets      map[string]Budget
	Acknowledged map[string]bool
	Demo         bool
}
type ImportOptions struct {
	Name      string
	From      string
	Through   string
	Confirmed bool
	Today     time.Time
}
type Preview struct {
	Snapshot Snapshot
	Net      Money
	Gross    Money
	Credits  Money
}

func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("cost is required")
	}
	negative := false
	if s[0] == '-' {
		negative = true
		s = s[1:]
	} else if s[0] == '+' {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("use a decimal amount, such as 12.340000")
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
		if frac == "" || len(frac) > 6 {
			return 0, errors.New("use one to six decimal places; finer precision must be converted explicitly")
		}
	}
	for _, c := range parts[0] + frac {
		if c < '0' || c > '9' {
			return 0, errors.New("cost must contain digits and an optional decimal point")
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > int64(MaxRow)/Scale {
		return 0, errors.New("amount exceeds the supported limit")
	}
	for len(frac) < 6 {
		frac += "0"
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	m := Money(whole*Scale + f)
	if m > MaxRow {
		return 0, errors.New("amount exceeds the supported limit")
	}
	if negative {
		m = -m
	}
	return m, nil
}
func Add(a, b Money) (Money, error) {
	if b > 0 && a > MaxTotal-b || b < 0 && a < -MaxTotal-b {
		return 0, errors.New("dataset total exceeds the supported limit")
	}
	return a + b, nil
}
func Ratio(m Money, n, d int64) Money {
	v := new(big.Int).Mul(big.NewInt(int64(m)), big.NewInt(n))
	neg := v.Sign() < 0
	v.Abs(v)
	v.Add(v, big.NewInt(d/2))
	v.Quo(v, big.NewInt(d))
	if neg {
		v.Neg(v)
	}
	return Money(v.Int64())
}
func Decimal(m Money) string {
	negative := m < 0
	if negative {
		m = -m
	}
	s := fmt.Sprintf("%d.%06d", m/Money(Scale), m%Money(Scale))
	if negative {
		s = "-" + s
	}
	return s
}
func Format(m Money, currency string) string {
	symbols := map[string]string{"USD": "$", "EUR": "€", "GBP": "£"}
	negative := m < 0
	if negative {
		m = -m
	}
	cents := (int64(m) + 5000) / 10000
	whole := strconv.FormatInt(cents/100, 10)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	s := symbols[currency] + whole + fmt.Sprintf(".%02d", cents%100)
	if negative {
		s = "−" + s
	}
	return s
}
func ValidCurrency(c string) bool { return c == "USD" || c == "EUR" || c == "GBP" }
func ValidProvider(p string) bool { return p == "aws" || p == "gcp" || p == "azure" }
func ProviderName(p string) string {
	switch p {
	case "aws":
		return "AWS"
	case "gcp":
		return "Google Cloud"
	case "azure":
		return "Azure"
	default:
		return "All providers"
	}
}
func Day(s string) (time.Time, error) {
	t, e := time.Parse("2006-01-02", s)
	if e != nil || t.Format("2006-01-02") != s {
		return time.Time{}, errors.New("use a date in YYYY-MM-DD format")
	}
	if t.Year() < 2000 || t.Year() > 2100 {
		return time.Time{}, errors.New("dates must be between 2000 and 2100")
	}
	return t, nil
}
func Month(s string) (time.Time, error) {
	t, e := time.Parse("2006-01", s)
	if e != nil || t.Format("2006-01") != s {
		return time.Time{}, errors.New("use a month in YYYY-MM format")
	}
	if t.Year() < 2000 || t.Year() > 2100 {
		return time.Time{}, errors.New("unsupported month")
	}
	return t, nil
}
func BudgetKey(month, provider, currency string) string {
	return month + "|" + provider + "|" + currency
}
func AlertKey(id, provider, service, day string) string {
	v := sha256.Sum256([]byte(id + "\x00" + provider + "\x00" + service + "\x00" + day))
	return hex.EncodeToString(v[:])
}
func Fingerprint(s Snapshot) string {
	rows := append([]Row(nil), s.Rows...)
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Cost < b.Cost
	})
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%t\n", s.Currency, s.Coverage.From, s.Coverage.Through, s.Coverage.Confirmed)
	for _, r := range rows {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\n", r.Date, r.Provider, r.Service, r.Currency, r.Cost)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func ValidateSnapshot(s Snapshot) error {
	if len(s.Rows) == 0 || len(s.Rows) > MaxRows {
		return errors.New("dataset must contain 1 to 25,000 rows")
	}
	if !ValidCurrency(s.Currency) {
		return errors.New("unsupported dataset currency")
	}
	from, e := Day(s.Coverage.From)
	if e != nil {
		return e
	}
	through, e := Day(s.Coverage.Through)
	if e != nil || through.Before(from) || through.Sub(from) > 366*24*time.Hour*5 {
		return errors.New("coverage must be an ordered interval of at most five years")
	}
	if len(s.Name) > 200 || !validText(s.Name) {
		return errors.New("invalid source name")
	}
	var total Money
	for _, r := range s.Rows {
		if _, e := Day(r.Date); e != nil {
			return e
		}
		if r.Date < s.Coverage.From || r.Date > s.Coverage.Through {
			return errors.New("all charge dates must be inside coverage")
		}
		if !ValidProvider(r.Provider) || r.Currency != s.Currency || strings.TrimSpace(r.Service) == "" || len(r.Service) > 200 || !validText(r.Service) || r.Cost > MaxRow || r.Cost < -MaxRow {
			return errors.New("invalid billing row")
		}
		abs := r.Cost
		if abs < 0 {
			abs = -abs
		}
		var err error
		total, err = Add(total, abs)
		if err != nil {
			return err
		}
	}
	if s.ID != Fingerprint(s) {
		return errors.New("dataset fingerprint does not match its contents")
	}
	return nil
}
func validText(s string) bool {
	for _, r := range s {
		if r == 0 || r < 32 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}
func ValidateWorkspace(w Workspace) error {
	if err := ValidateSnapshot(w.Active); err != nil {
		return err
	}
	if len(w.Budgets) > 1000 || len(w.Acknowledged) > MaxRows {
		return errors.New("saved state exceeds supported limits")
	}
	for key, b := range w.Budgets {
		if _, e := Month(b.Month); e != nil {
			return e
		}
		if b.Provider != "" && !ValidProvider(b.Provider) || !ValidCurrency(b.Currency) || b.Limit < 0 || b.Limit > MaxRow || key != BudgetKey(b.Month, b.Provider, b.Currency) {
			return errors.New("invalid saved budget")
		}
	}
	for key, value := range w.Acknowledged {
		if len(key) != 64 || !value {
			return errors.New("invalid saved acknowledgment")
		}
		if _, e := hex.DecodeString(key); e != nil {
			return errors.New("invalid saved acknowledgment")
		}
	}
	return nil
}
func Replace(w Workspace, s Snapshot) Workspace {
	if w.Active.ID != s.ID {
		w.Acknowledged = map[string]bool{}
	}
	w.Active = s
	w.Demo = false
	return w
}
