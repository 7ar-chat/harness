package bench

import (
	"fmt"
	"strings"
)

// LongTasks returns the long-session tasks: they need 50+ steps and, with a small
// context window, compaction and masking (set LLM_CONTEXT_WINDOWS=<model>=32000 to
// force it). They are generated programmatically so the repositories are big
// enough to be explored and edited file by file.
func LongTasks() []Task {
	ts := []Task{migrateAPIV2(), fixSharedHelperBug(), featureAcrossEightFiles()}
	for i := range ts {
		ts[i].Tier = TierLong
	}
	return ts
}

// ---- go-migrate-api-v2 ----

const migrateModules = 28

func migrateName(k int) string { return fmt.Sprintf("m%02d", k) }

// migrateModule is the source of module k: v1 (api.Get) or v2 (api.GetV2). Every
// fourth module has a second call site and a helper file.
func migrateModule(k int, v2 bool) string {
	n := migrateName(k)
	var b strings.Builder
	imports := `"strings"` + "\n\t" + `"mig/api"`
	if v2 {
		imports = `"context"` + "\n\t" + `"strings"` + "\n\n\t" + `"mig/api"`
	}
	fmt.Fprintf(&b, "// Package %s labels records of the %s feed.\npackage %s\n\nimport (\n\t%s\n)\n\n", n, n, n, imports)
	call := func(fn, key string) string {
		if !v2 {
			return fmt.Sprintf("strings.ToUpper(api.Get(%s))", key)
		}
		return "" // v2 uses a block, see below
	}
	fn := func(name, doc, prefix string) {
		fmt.Fprintf(&b, "// %s %s\nfunc %s(id string) string {\n", name, doc, name)
		if !v2 {
			fmt.Fprintf(&b, "\treturn %s\n}\n\n", call("", fmt.Sprintf("%q + id", prefix)))
			return
		}
		fmt.Fprintf(&b, "\tr, err := api.GetV2(context.Background(), %q+id)\n\tif err != nil {\n\t\treturn \"\"\n\t}\n\treturn strings.ToUpper(r.Value)\n}\n\n", prefix)
	}
	fn("Label", "returns the display label of id.", n+"/")
	if k%4 == 3 {
		fn("Title", "returns the title of id.", n+"/title/")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func migrateTest(k int) string {
	n := migrateName(k)
	s := fmt.Sprintf("package %s\n\nimport \"testing\"\n\nfunc TestLabel(t *testing.T) {\n\tif got, want := Label(\"x\"), \"VAL(%s/X)\"; got != want {\n\t\tt.Fatalf(\"Label = %%q, want %%q\", got, want)\n\t}\n}\n", n, strings.ToUpper(n))
	if k%4 == 3 {
		s += fmt.Sprintf("\nfunc TestTitle(t *testing.T) {\n\tif got, want := Title(\"y\"), \"VAL(%s/TITLE/Y)\"; got != want {\n\t\tt.Fatalf(\"Title = %%q, want %%q\", got, want)\n\t}\n}\n", strings.ToUpper(n))
	}
	return s
}

const apiV1AndV2 = `// Package api is the feed client. Get (v1) is deprecated and removed in the next release.
package api

import "context"

// Result is the v2 answer.
type Result struct{ Value string }

// Get returns the value of id.
//
// Deprecated: use GetV2.
func Get(id string) string { return "val(" + id + ")" }

// GetV2 returns the value of id; it fails when ctx is done.
func GetV2(ctx context.Context, id string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Value: "val(" + id + ")"}, nil
}
`

const apiV2Only = `// Package api is the feed client.
package api

import "context"

// Result is the answer.
type Result struct{ Value string }

// GetV2 returns the value of id; it fails when ctx is done.
func GetV2(ctx context.Context, id string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Value: "val(" + id + ")"}, nil
}
`

// migrationGuide is a long guide (about 25 KB) the agent is told to follow: reading it costs context.
func migrationGuide() string {
	var b strings.Builder
	b.WriteString("# Feed API v2 migration guide\n\nReplace every call of api.Get(id) by api.GetV2(ctx, id), which returns (api.Result, error).\n" +
		"Use context.Background() where the function has no context. On error return the zero value the function returned for a missing record (\"\" for strings). Use r.Value for the old string.\n" +
		"Do not change function signatures, exported names or tests: only the call sites.\n\n## Changelog\n")
	for i := 1; b.Len() < 25000; i++ {
		fmt.Fprintf(&b, "- 2.%d.%d: feed shard %d now answers in %d ms at the 99th percentile; clients should not retry within %d ms.\n", i/10, i%10, i, 20+i%40, 100+i%9*50)
	}
	return b.String()
}

func migrateAPIV2() Task {
	files := map[string]string{
		"go.mod":         "module mig\n\ngo 1.21\n",
		"AGENTS.md":      "# Notes\n\nBuild and test: go build ./... && go test ./...\nThe v1 call api.Get is removed in the next release. The migration guide is docs/api-v2.md; follow it.\nKeep gofmt clean.\n",
		"docs/api-v2.md": migrationGuide(),
		"api/api.go":     apiV1AndV2,
	}
	solution := map[string]string{}
	for k := 0; k < migrateModules; k++ {
		n := migrateName(k)
		files[n+"/"+n+".go"] = migrateModule(k, false)
		files[n+"/"+n+"_test.go"] = migrateTest(k)
		solution[n+"/"+n+".go"] = migrateModule(k, true)
	}
	return Task{
		Name: "go-migrate-api-v2",
		Prompt: "This Go module has 28 packages (m00 to m27) that call the deprecated api.Get. Migrate every call to api.GetV2 following docs/api-v2.md; " +
			"the old function is deleted after you are done, so nothing may use it. Keep the behaviour and the tests." + noAsking,
		Needs:    []string{"go"},
		Files:    files,
		Hidden:   map[string]string{"api/api.go": apiV2Only}, // v1 removed: an unmigrated package no longer compiles
		Verify:   "gofmt -l . | (! grep .) && go build ./... && go vet ./... && go test ./...",
		Solution: solution,
	}
}

// ---- go-fix-shared-helper ----

const failPackages = 40

// failingPackages use util.Clamp; their tests fail together because of one bug in util.
var failingPackages = map[int]bool{5: true, 17: true, 23: true, 31: true}

func fixSharedHelperBug() Task {
	utilGood := "// Package util has helpers shared by the packages.\npackage util\n\n" +
		"// Clamp limits v to [lo, hi].\nfunc Clamp(v, lo, hi int) int {\n\tif v < lo {\n\t\treturn lo\n\t}\n\tif v > hi {\n\t\treturn hi\n\t}\n\treturn v\n}\n\n" +
		"// Abs is the absolute value of v.\nfunc Abs(v int) int {\n\tif v < 0 {\n\t\treturn -v\n\t}\n\treturn v\n}\n"
	utilBad := strings.Replace(utilGood, "if v > hi {\n\t\treturn hi", "if v >= hi {\n\t\treturn hi - 1", 1)
	utilTest := "package util\n\nimport \"testing\"\n\nfunc TestClamp(t *testing.T) {\n\tfor _, c := range []struct{ v, lo, hi, want int }{{5, 0, 10, 5}, {-1, 0, 10, 0}, {11, 0, 10, 10}, {10, 0, 10, 10}} {\n\t\tif got := Clamp(c.v, c.lo, c.hi); got != c.want {\n\t\t\tt.Errorf(\"Clamp(%d,%d,%d) = %d, want %d\", c.v, c.lo, c.hi, got, c.want)\n\t\t}\n\t}\n}\n"
	files := map[string]string{
		"go.mod":       "module many\n\ngo 1.21\n",
		"AGENTS.md":    "# Notes\n\nRun everything with: go test ./...\nPackages p00..p39 are independent features; shared helpers live in util.\n",
		"util/util.go": utilBad,
	}
	hidden := map[string]string{"util/util_test.go": utilTest}
	for k := 0; k < failPackages; k++ {
		n := fmt.Sprintf("p%02d", k)
		var src, test string
		if failingPackages[k] {
			src = fmt.Sprintf("// Package %s scores readings.\npackage %s\n\nimport \"many/util\"\n\n// Score is the reading limited to the 0..100 range, plus %d.\nfunc Score(v int) int { return util.Clamp(v, 0, 100) + %d }\n", n, n, k, k)
			test = fmt.Sprintf("package %s\n\nimport \"testing\"\n\nfunc TestScore(t *testing.T) {\n\tfor in, want := range map[int]int{50: %d, -5: %d, 100: %d, 250: %d} {\n\t\tif got := Score(in); got != want {\n\t\t\tt.Errorf(\"Score(%%d) = %%d, want %%d\", in, got, want)\n\t\t}\n\t}\n}\n", n, 50+k, k, 100+k, 100+k)
		} else {
			src = fmt.Sprintf("// Package %s sums readings.\npackage %s\n\n// Sum adds the readings and %d.\nfunc Sum(xs []int) int {\n\tt := %d\n\tfor _, x := range xs {\n\t\tt += x\n\t}\n\treturn t\n}\n", n, n, k, k)
			test = fmt.Sprintf("package %s\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif got, want := Sum([]int{1, 2, 3}), %d; got != want {\n\t\tt.Fatalf(\"Sum = %%d, want %%d\", got, want)\n\t}\n}\n", n, 6+k)
		}
		files[n+"/"+n+".go"] = src
		files[n+"/"+n+"_test.go"] = test
	}
	return Task{
		Name:     "go-fix-shared-helper",
		Prompt:   "go test ./... fails in several of the 40 packages of this module. Find the cause and fix it without changing any test." + noAsking,
		Needs:    []string{"go"},
		Files:    files,
		Hidden:   hidden,
		Verify:   "go vet ./... && go test ./...",
		Solution: map[string]string{"util/util.go": utilGood},
	}
}

// ---- go-feature-across-eight-files ----

const currencyAgents = `# Notes for agents

Test: go test ./... ; format: gofmt -l . must print nothing.

Conventions (reviewers check them):
- Money is integer cents (int64) with a currency code. Never use float64 outside package money.
- Error messages start with the package name and a colon, lowercase, and wrap the cause with %w.
- Every exported identifier has a doc comment that starts with its name.
- Every package you touch has table-driven tests in <pkg>_test.go (a struct slice named cases, one t.Run per case).
- Supported currencies are in money/money.go; do not add others.
- Leave no TODO(currency) comment behind.
`

func featureAcrossEightFiles() Task {
	files := map[string]string{
		"go.mod":    "module shop\n\ngo 1.21\n",
		"AGENTS.md": currencyAgents,
		"money/money.go": `// Package money holds amounts in integer cents.
package money

import "errors"

// Money is an amount in the minor unit of its currency.
type Money struct {
	Cents int64
	Cur   string
}

// perEUR is the value of one euro in each supported currency.
var perEUR = map[string]float64{"EUR": 1, "USD": 1.10, "GBP": 0.85, "CHF": 0.95}

var errTodo = errors.New("money: not implemented")

// Convert returns m in currency to, rounded to the nearest cent.
// TODO(currency): convert through EUR; unknown currencies are errors.
func Convert(m Money, to string) (Money, error) { return Money{}, errTodo }

// Format renders m as "12.34 USD" (a minus sign before negative amounts).
// TODO(currency): implement.
func Format(m Money) string { return "" }
`,
		"cart/cart.go": `// Package cart is a list of priced items.
package cart

import (
	"errors"

	"shop/money"
)

// Item is a quantity of one priced product.
type Item struct {
	Price money.Money
	Qty   int
}

// Cart is the items a customer wants.
type Cart struct{ Items []Item }

// Total sums the cart in currency cur.
// TODO(currency): convert every price to cur.
func (c Cart) Total(cur string) (money.Money, error) {
	return money.Money{}, errors.New("cart: not implemented")
}
`,
		"order/order.go": `// Package order turns a cart into something to charge.
package order

import (
	"errors"

	"shop/cart"
	"shop/money"
)

// Order is a cart bought in a currency.
type Order struct {
	ID   string
	Cart cart.Cart
	Cur  string
}

// Charge is what the customer pays: the cart total in the order currency.
// TODO(currency): wrap errors with the order id.
func (o Order) Charge() (money.Money, error) {
	return money.Money{}, errors.New("order: not implemented")
}
`,
		"invoice/invoice.go": `// Package invoice renders orders as text.
package invoice

import (
	"errors"

	"shop/order"
)

// Render returns one line per item ("2 x 10.00 USD", prices in the order currency), then "TOTAL 20.00 USD".
// TODO(currency): implement.
func Render(o order.Order) (string, error) { return "", errors.New("invoice: not implemented") }
`,
		"report/report.go": `// Package report summarises many orders.
package report

import (
	"errors"

	"shop/money"
	"shop/order"
)

// Summary is the sum of the orders, in currency cur.
// TODO(currency): implement.
func Summary(orders []order.Order, cur string) (money.Money, error) {
	return money.Money{}, errors.New("report: not implemented")
}
`,
		"export/export.go": `// Package export writes orders for other systems.
package export

import (
	"errors"

	"shop/order"
)

// CSV returns "id,amount,currency" as the header and one line per order with its charge, e.g. "A1,12.34,USD".
// TODO(currency): implement.
func CSV(orders []order.Order) (string, error) { return "", errors.New("export: not implemented") }
`,
		"api/api.go": `// Package api serves order totals over HTTP.
package api

import (
	"net/http"

	"shop/order"
)

// Total answers GET ?cur=USD with {"total":"12.34","currency":"USD"} (the order currency when cur is absent),
// and 400 with {"error":"<message>"} when the currency is unknown.
// TODO(currency): implement.
func Total(o order.Order) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not implemented", 501) })
}
`,
		"cli/cli.go": `// Package cli is the command line of the shop.
package cli

import (
	"errors"
	"io"

	"shop/order"
)

// Run executes "total [--cur CODE]" for o, printing "12.34 USD" and a newline. Other commands are errors.
// TODO(currency): implement.
func Run(args []string, o order.Order, out io.Writer) error { return errors.New("cli: not implemented") }
`,
	}
	solution := map[string]string{
		"money/money.go": `// Package money holds amounts in integer cents.
package money

import (
	"fmt"
	"math"
)

// Money is an amount in the minor unit of its currency.
type Money struct {
	Cents int64
	Cur   string
}

// perEUR is the value of one euro in each supported currency.
var perEUR = map[string]float64{"EUR": 1, "USD": 1.10, "GBP": 0.85, "CHF": 0.95}

// Convert returns m in currency to, rounded to the nearest cent.
func Convert(m Money, to string) (Money, error) {
	from, ok := perEUR[m.Cur]
	if !ok {
		return Money{}, fmt.Errorf("money: unknown currency %q", m.Cur)
	}
	dst, ok := perEUR[to]
	if !ok {
		return Money{}, fmt.Errorf("money: unknown currency %q", to)
	}
	if m.Cur == to {
		return m, nil
	}
	return Money{Cents: int64(math.Round(float64(m.Cents) / from * dst)), Cur: to}, nil
}

// Format renders m as "12.34 USD" (a minus sign before negative amounts).
func Format(m Money) string {
	sign, c := "", m.Cents
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, c/100, c%100, m.Cur)
}
`,
		"money/money_test.go": `package money

import "testing"

func TestConvertAndFormat(t *testing.T) {
	cases := []struct {
		name string
		in   Money
		to   string
		want string
	}{
		{"same", Money{1000, "EUR"}, "EUR", "10.00 EUR"},
		{"up", Money{1000, "EUR"}, "USD", "11.00 USD"},
		{"back", Money{1100, "USD"}, "EUR", "10.00 EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Convert(c.in, c.to)
			if err != nil || Format(got) != c.want {
				t.Fatalf("got %v %v, want %s", got, err, c.want)
			}
		})
	}
}
`,
		"cart/cart.go": `// Package cart is a list of priced items.
package cart

import (
	"fmt"

	"shop/money"
)

// Item is a quantity of one priced product.
type Item struct {
	Price money.Money
	Qty   int
}

// Cart is the items a customer wants.
type Cart struct{ Items []Item }

// Total sums the cart in currency cur.
func (c Cart) Total(cur string) (money.Money, error) {
	total := money.Money{Cur: cur}
	for _, it := range c.Items {
		p, err := money.Convert(it.Price, cur)
		if err != nil {
			return money.Money{}, fmt.Errorf("cart: %w", err)
		}
		total.Cents += p.Cents * int64(it.Qty)
	}
	return total, nil
}
`,
		"cart/cart_test.go": `package cart

import (
	"testing"

	"shop/money"
)

func TestTotal(t *testing.T) {
	cases := []struct {
		name  string
		items []Item
		cur   string
		want  int64
	}{
		{"empty", nil, "EUR", 0},
		{"mixed", []Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 2}, {Price: money.Money{Cents: 1100, Cur: "USD"}, Qty: 1}}, "EUR", 3000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Cart{c.items}.Total(c.cur)
			if err != nil || got.Cents != c.want {
				t.Fatalf("got %v %v, want %d", got, err, c.want)
			}
		})
	}
}
`,
		"order/order.go": `// Package order turns a cart into something to charge.
package order

import (
	"fmt"

	"shop/cart"
	"shop/money"
)

// Order is a cart bought in a currency.
type Order struct {
	ID   string
	Cart cart.Cart
	Cur  string
}

// Charge is what the customer pays: the cart total in the order currency.
func (o Order) Charge() (money.Money, error) {
	t, err := o.Cart.Total(o.Cur)
	if err != nil {
		return money.Money{}, fmt.Errorf("order %s: %w", o.ID, err)
	}
	return t, nil
}
`,
		"order/order_test.go": `package order

import (
	"testing"

	"shop/cart"
	"shop/money"
)

func TestCharge(t *testing.T) {
	cases := []struct {
		name string
		cur  string
		ok   bool
	}{{"known", "USD", true}, {"unknown", "XXX", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := Order{ID: "A1", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 100, Cur: "EUR"}, Qty: 1}}}, Cur: c.cur}
			if _, err := o.Charge(); (err == nil) != c.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
`,
		"invoice/invoice.go": `// Package invoice renders orders as text.
package invoice

import (
	"fmt"
	"strings"

	"shop/money"
	"shop/order"
)

// Render returns one line per item ("2 x 10.00 USD", prices in the order currency), then "TOTAL 20.00 USD".
func Render(o order.Order) (string, error) {
	var b strings.Builder
	for _, it := range o.Cart.Items {
		p, err := money.Convert(it.Price, o.Cur)
		if err != nil {
			return "", fmt.Errorf("invoice: %w", err)
		}
		fmt.Fprintf(&b, "%d x %s\n", it.Qty, money.Format(p))
	}
	total, err := o.Charge()
	if err != nil {
		return "", fmt.Errorf("invoice: %w", err)
	}
	b.WriteString("TOTAL " + money.Format(total) + "\n")
	return b.String(), nil
}
`,
		"invoice/invoice_test.go": `package invoice

import (
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestRender(t *testing.T) {
	cases := []struct {
		name string
		cur  string
		want string
	}{{"eur", "EUR", "2 x 10.00 EUR\nTOTAL 20.00 EUR\n"}, {"usd", "USD", "2 x 11.00 USD\nTOTAL 22.00 USD\n"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := order.Order{ID: "A", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 2}}}, Cur: c.cur}
			if got, err := Render(o); err != nil || got != c.want {
				t.Fatalf("got %q %v", got, err)
			}
		})
	}
}
`,
		"report/report.go": `// Package report summarises many orders.
package report

import (
	"fmt"

	"shop/money"
	"shop/order"
)

// Summary is the sum of the orders, in currency cur.
func Summary(orders []order.Order, cur string) (money.Money, error) {
	sum := money.Money{Cur: cur}
	for _, o := range orders {
		t, err := o.Cart.Total(cur)
		if err != nil {
			return money.Money{}, fmt.Errorf("report: %w", err)
		}
		sum.Cents += t.Cents
	}
	return sum, nil
}
`,
		"report/report_test.go": `package report

import (
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestSummary(t *testing.T) {
	one := order.Order{ID: "A", Cur: "EUR", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 1}}}}
	cases := []struct {
		name   string
		orders []order.Order
		want   int64
	}{{"none", nil, 0}, {"two", []order.Order{one, one}, 2200}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Summary(c.orders, "USD")
			if err != nil || got.Cents != c.want {
				t.Fatalf("got %v %v", got, err)
			}
		})
	}
}
`,
		"export/export.go": `// Package export writes orders for other systems.
package export

import (
	"fmt"
	"strings"

	"shop/money"
	"shop/order"
)

// CSV returns "id,amount,currency" as the header and one line per order with its charge, e.g. "A1,12.34,USD".
func CSV(orders []order.Order) (string, error) {
	var b strings.Builder
	b.WriteString("id,amount,currency\n")
	for _, o := range orders {
		c, err := o.Charge()
		if err != nil {
			return "", fmt.Errorf("export: %w", err)
		}
		f := strings.Fields(money.Format(c))
		fmt.Fprintf(&b, "%s,%s,%s\n", o.ID, f[0], f[1])
	}
	return b.String(), nil
}
`,
		"export/export_test.go": `package export

import (
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestCSV(t *testing.T) {
	o := order.Order{ID: "A1", Cur: "USD", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 1}}}}
	cases := []struct {
		name   string
		orders []order.Order
		want   string
	}{{"empty", nil, "id,amount,currency\n"}, {"one", []order.Order{o}, "id,amount,currency\nA1,11.00,USD\n"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := CSV(c.orders); err != nil || got != c.want {
				t.Fatalf("got %q %v", got, err)
			}
		})
	}
}
`,
		"api/api.go": `// Package api serves order totals over HTTP.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"shop/money"
	"shop/order"
)

// Total answers GET ?cur=USD with {"total":"12.34","currency":"USD"} (the order currency when cur is absent),
// and 400 with {"error":"<message>"} when the currency is unknown.
func Total(o order.Order) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := r.URL.Query().Get("cur")
		if cur == "" {
			cur = o.Cur
		}
		w.Header().Set("Content-Type", "application/json")
		t, err := o.Cart.Total(cur)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"total": strings.Fields(money.Format(t))[0], "currency": cur})
	})
}
`,
		"api/api_test.go": `package api

import (
	"net/http/httptest"
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestTotal(t *testing.T) {
	o := order.Order{ID: "A", Cur: "EUR", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 1}}}}
	cases := []struct {
		name, query string
		code        int
	}{{"default", "", 200}, {"usd", "?cur=USD", 200}, {"unknown", "?cur=XXX", 400}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Total(o).ServeHTTP(rec, httptest.NewRequest("GET", "/"+c.query, nil))
			if rec.Code != c.code {
				t.Fatalf("code %d", rec.Code)
			}
		})
	}
}
`,
		"cli/cli.go": `// Package cli is the command line of the shop.
package cli

import (
	"fmt"
	"io"

	"shop/money"
	"shop/order"
)

// Run executes "total [--cur CODE]" for o, printing "12.34 USD" and a newline. Other commands are errors.
func Run(args []string, o order.Order, out io.Writer) error {
	if len(args) == 0 || args[0] != "total" {
		return fmt.Errorf("cli: unknown command %q", args)
	}
	cur := o.Cur
	if len(args) == 3 && args[1] == "--cur" {
		cur = args[2]
	}
	t, err := o.Cart.Total(cur)
	if err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	_, err = fmt.Fprintln(out, money.Format(t))
	return err
}
`,
		"cli/cli_test.go": `package cli

import (
	"bytes"
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestRun(t *testing.T) {
	o := order.Order{ID: "A", Cur: "EUR", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 1}}}}
	cases := []struct {
		name string
		args []string
		want string
		ok   bool
	}{{"default", []string{"total"}, "10.00 EUR\n", true}, {"usd", []string{"total", "--cur", "USD"}, "11.00 USD\n", true}, {"other", []string{"x"}, "", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := Run(c.args, o, &out); (err == nil) != c.ok || out.String() != c.want {
				t.Fatalf("err %v out %q", err, out.String())
			}
		})
	}
}
`,
	}
	// Hidden tests pin the behaviour of the prompt, in files the agent never sees.
	imp := func(pkg string, others ...string) string {
		out := "package " + pkg + "\n\nimport (\n\t\"strings\"\n\t\"testing\"\n\n"
		for _, o := range others {
			out += "\t\"shop/" + o + "\"\n"
		}
		return out + ")\n\nvar _ = strings.Contains\n\n"
	}
	const ord = `func sample() order.Order {
	return order.Order{ID: "A1", Cur: "USD", Cart: cart.Cart{Items: []cart.Item{
		{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 2}, {Price: money.Money{Cents: 850, Cur: "GBP"}, Qty: 1}}}}
}
`
	hidden := map[string]string{
		"money/hidden_test.go": imp("money") + `func TestHidden(t *testing.T) {
	got, err := Convert(Money{1000, "EUR"}, "GBP")
	if err != nil || got != (Money{850, "GBP"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := Convert(Money{1, "EUR"}, "XXX"); err == nil || !strings.HasPrefix(err.Error(), "money:") {
		t.Fatalf("err %v", err)
	}
	if s := Format(Money{-1234, "USD"}); s != "-12.34 USD" {
		t.Fatal(s)
	}
	if s := Format(Money{5, "EUR"}); s != "0.05 EUR" {
		t.Fatal(s)
	}
}
`,
		"cart/hidden_test.go": imp("cart", "money") + `func TestHidden(t *testing.T) {
	c := Cart{Items: []Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 3}, {Price: money.Money{Cents: 850, Cur: "GBP"}, Qty: 1}}}
	got, err := c.Total("EUR")
	if err != nil || got != (money.Money{Cents: 4000, Cur: "EUR"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := c.Total("XXX"); err == nil || !strings.HasPrefix(err.Error(), "cart: money:") {
		t.Fatalf("err %v", err)
	}
}
`,
		"order/hidden_test.go": imp("order", "cart", "money") + `func sample() Order {
	return Order{ID: "A1", Cur: "USD", Cart: cart.Cart{Items: []cart.Item{
		{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 2}, {Price: money.Money{Cents: 850, Cur: "GBP"}, Qty: 1}}}}
}

func TestHidden(t *testing.T) {
	got, err := sample().Charge()
	if err != nil || got != (money.Money{Cents: 3300, Cur: "USD"}) {
		t.Fatalf("%v %v", got, err)
	}
	o := sample()
	o.Cur = "XXX"
	if _, err := o.Charge(); err == nil || !strings.HasPrefix(err.Error(), "order A1: ") {
		t.Fatalf("err %v", err)
	}
}
`,
		"invoice/hidden_test.go": imp("invoice", "cart", "money", "order") + ord + `func TestHidden(t *testing.T) {
	got, err := Render(sample())
	want := "2 x 11.00 USD\n1 x 11.00 USD\nTOTAL 33.00 USD\n"
	if err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}
}
`,
		"report/hidden_test.go": imp("report", "cart", "money", "order") + ord + `func TestHidden(t *testing.T) {
	got, err := Summary([]order.Order{sample(), sample()}, "EUR")
	if err != nil || got != (money.Money{Cents: 6000, Cur: "EUR"}) {
		t.Fatalf("%v %v", got, err)
	}
}
`,
		"export/hidden_test.go": imp("export", "cart", "money", "order") + ord + `func TestHidden(t *testing.T) {
	got, err := CSV([]order.Order{sample()})
	if err != nil || got != "id,amount,currency\nA1,33.00,USD\n" {
		t.Fatalf("%q %v", got, err)
	}
}
`,
		"api/hidden_test.go": `package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestHidden(t *testing.T) {
	o := order.Order{ID: "A1", Cur: "USD", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 2}}}}
	rec := httptest.NewRecorder()
	Total(o).ServeHTTP(rec, httptest.NewRequest("GET", "/?cur=GBP", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != ` + "`" + `{"currency":"GBP","total":"17.00"}` + "`" + ` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	Total(o).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), ` + "`" + `"total":"22.00"` + "`" + `) {
		t.Fatalf("%q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	Total(o).ServeHTTP(rec, httptest.NewRequest("GET", "/?cur=XXX", nil))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), ` + "`" + `"error"` + "`" + `) {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}
`,
		"cli/hidden_test.go": `package cli

import (
	"bytes"
	"strings"
	"testing"

	"shop/cart"
	"shop/money"
	"shop/order"
)

func TestHidden(t *testing.T) {
	o := order.Order{ID: "A1", Cur: "USD", Cart: cart.Cart{Items: []cart.Item{{Price: money.Money{Cents: 1000, Cur: "EUR"}, Qty: 2}}}}
	var out bytes.Buffer
	if err := Run([]string{"total", "--cur", "CHF"}, o, &out); err != nil || out.String() != "19.00 CHF\n" {
		t.Fatalf("%v %q", err, out.String())
	}
	if err := Run([]string{"nope"}, o, &out); err == nil || !strings.HasPrefix(err.Error(), "cli:") {
		t.Fatalf("err %v", err)
	}
}
`,
	}
	return Task{
		Name: "go-feature-across-eight-files",
		Prompt: "Implement currency support for the shop: eight functions across eight packages are stubs marked TODO(currency) (money, cart, order, invoice, report, export, api, cli). " +
			"Implement them as their doc comments say, and follow the conventions in AGENTS.md exactly (they are checked)." + noAsking,
		Needs:  []string{"go"},
		Files:  files,
		Hidden: hidden,
		Verify: `gofmt -l . | grep -v hidden_test.go | (! grep .) && go vet ./... && go test ./... && ` +
			`! grep -rn "TODO(currency)" --include=*.go . && ` +
			`for d in money cart order invoice report export api cli; do ls $d/*_test.go | grep -v hidden_test.go | grep -q . || { echo "no tests in $d"; exit 1; }; done && ` +
			`! grep -rnE "float(32|64)" --include=*.go cart order invoice report export api cli | grep -v _test.go`,
		Solution: solution,
	}
}
