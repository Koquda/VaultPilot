package billing

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

func ParseCSV(input io.Reader, options ImportOptions) (Preview, error) {
	var result Preview
	from, err := Day(options.From)
	if err != nil {
		return result, fmt.Errorf("coverage start: %w", err)
	}
	through, err := Day(options.Through)
	if err != nil || through.Before(from) {
		return result, errors.New("coverage end must be on or after its start")
	}
	today := options.Today.UTC().Format("2006-01-02")
	if options.Today.IsZero() {
		today = time.Now().UTC().Format("2006-01-02")
	}
	if options.Through > today {
		return result, errors.New("coverage cannot extend into a future billing day")
	}
	data, err := io.ReadAll(io.LimitReader(input, MaxBytes+1))
	if err != nil {
		return result, err
	}
	if len(data) > MaxBytes {
		return result, errors.New("CSV exceeds the 2 MB limit")
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return result, errors.New("a CSV header is required")
	}
	wanted := map[string]bool{"date": true, "provider": true, "service": true, "currency": true, "cost": true}
	columns := map[string]int{}
	if len(headers) != 5 {
		return result, errors.New("CSV needs exactly date, provider, service, currency and cost columns")
	}
	for i, name := range headers {
		name = strings.TrimSpace(name)
		if !wanted[name] {
			return result, fmt.Errorf("unknown column %q", name)
		}
		if _, ok := columns[name]; ok {
			return result, fmt.Errorf("duplicate column %q", name)
		}
		columns[name] = i
	}
	name := filepath.Base(options.Name)
	if name == "." || name == "" {
		name = "Pasted billing CSV"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	s := Snapshot{Name: name, Coverage: Coverage{options.From, options.Through, options.Confirmed}, ImportedAt: options.Today.UTC()}
	if s.ImportedAt.IsZero() {
		s.ImportedAt = time.Now().UTC()
	}
	var problems []string
	bad := 0
	var absolute Money
	addProblem := func(row int, reason string) {
		bad++
		if len(problems) < 20 {
			problems = append(problems, fmt.Sprintf("Row %d: %s", row, reason))
		}
	}
	for index := 2; ; index++ {
		fields, e := reader.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return result, fmt.Errorf("CSV near row %d: %w", index, e)
		}
		if len(s.Rows)+bad >= MaxRows {
			return result, errors.New("CSV exceeds 25,000 rows")
		}
		if len(fields) != 5 {
			addProblem(index, "expected five fields")
			continue
		}
		get := func(key string) string { return strings.TrimSpace(fields[columns[key]]) }
		r := Row{Date: get("date"), Provider: strings.ToLower(get("provider")), Service: get("service"), Currency: strings.ToUpper(get("currency"))}
		if _, e := Day(r.Date); e != nil {
			addProblem(index, "invalid ISO date")
			continue
		}
		if r.Date < options.From || r.Date > options.Through || r.Date > today {
			addProblem(index, "charge date is outside coverage or in the future")
			continue
		}
		if !ValidProvider(r.Provider) {
			addProblem(index, "provider must be aws, azure or gcp")
			continue
		}
		if r.Service == "" || len(r.Service) > 200 || !validText(r.Service) {
			addProblem(index, "service must contain 1 to 200 valid characters")
			continue
		}
		if !ValidCurrency(r.Currency) {
			addProblem(index, "currency must be USD, EUR or GBP")
			continue
		}
		r.Cost, e = ParseMoney(get("cost"))
		if e != nil {
			addProblem(index, e.Error())
			continue
		}
		if s.Currency == "" {
			s.Currency = r.Currency
		}
		if r.Currency != s.Currency {
			addProblem(index, "mixed currencies are not supported")
			continue
		}
		a := r.Cost
		if a < 0 {
			a = -a
		}
		absolute, e = Add(absolute, a)
		if e != nil {
			return result, e
		}
		s.Rows = append(s.Rows, r)
	}
	if bad > 0 {
		return result, fmt.Errorf("%d invalid row(s). Nothing was imported.\n%s", bad, strings.Join(problems, "\n"))
	}
	if len(s.Rows) == 0 {
		return result, errors.New("CSV contains no billing rows")
	}
	s.ID = Fingerprint(s)
	if err := ValidateSnapshot(s); err != nil {
		return result, err
	}
	result.Snapshot = s
	for _, r := range s.Rows {
		result.Net += r.Cost
		if r.Cost >= 0 {
			result.Gross += r.Cost
		} else {
			result.Credits += r.Cost
		}
	}
	return result, nil
}
func ExportCSV(output io.Writer, rows []Row) error {
	w := csv.NewWriter(output)
	if e := w.Write([]string{"date", "provider", "service", "currency", "cost"}); e != nil {
		return e
	}
	for _, r := range rows {
		service := r.Service
		if len(service) > 0 && strings.ContainsRune("=+-@\t\r", rune(service[0])) {
			service = "'" + service
		}
		if e := w.Write([]string{r.Date, r.Provider, service, r.Currency, Decimal(r.Cost)}); e != nil {
			return e
		}
	}
	w.Flush()
	return w.Error()
}
