package pgstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Backend {
		s, pool := newPGPool(t)
		return conformance.Backend{
			Store: s,
			// Every timestamp of the schema moves into the past, so ages computed from now() see d more.
			Advance: func(t *testing.T, d time.Duration) {
				ctx := context.Background()
				rows, err := pool.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
					WHERE table_schema = current_schema() AND data_type = 'timestamp with time zone'`)
				if err != nil {
					t.Fatal(err)
				}
				var stmts []string
				for rows.Next() {
					var table, col string
					if err := rows.Scan(&table, &col); err != nil {
						t.Fatal(err)
					}
					stmts = append(stmts, fmt.Sprintf(`UPDATE %q SET %q = %q - make_interval(secs => %f)`, table, col, col, d.Seconds()))
				}
				rows.Close()
				for _, q := range stmts {
					if _, err := pool.Exec(ctx, q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
			},
		}
	})
}
