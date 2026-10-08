package chdb

import (
	"context"
	"os"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestOpNamesQueriesClickHouse checks that queries reach ClickHouse's query log named
// "<service>/<operation>", that an Op inside another keeps the outer one's settings, and that the
// service's metrics count them. It needs a server, so it only runs when TOPICFEED_CLICKHOUSE_TEST is
// set (plus the CLICKHOUSE_ADDR, CLICKHOUSE_USER and CLICKHOUSE_PASSWORD that reach it); it only
// reads.
func TestOpNamesQueriesClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	conn, err := Open(ctx, FromEnv().For("chdbtest"))
	if err != nil {
		t.Fatal(err)
	}
	outer := Op(ctx, "outer", clickhouse.Settings{"max_query_size": 1 << 20})
	var got []struct {
		Size string `ch:"size"`
	}
	if err := conn.Select(Op(outer, "inner"), &got, "SELECT toString(getSetting('max_query_size')) AS size FROM numbers(1000)  LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Size != "1048576" {
		t.Errorf("max_query_size inside the inner Op: %v, want 1048576", got)
	}
	var one uint8
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"outer.inner", "other"} {
		var m dto.Metric
		if err := metricQuerySeconds.WithLabelValues(op).(prometheus.Histogram).Write(&m); err != nil || m.GetHistogram().GetSampleCount() == 0 {
			t.Errorf("no query time recorded for %s (%v)", op, err)
		}
	}
	var rows dto.Metric
	if err := metricReadRows.WithLabelValues("outer.inner").Write(&rows); err != nil || rows.GetCounter().GetValue() < 1 {
		t.Errorf("read rows for outer.inner: %v (%v), want at least 1", rows.GetCounter().GetValue(), err)
	}

	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Skipf("can't flush the query log to check it: %v", err)
	}
	var comments []struct {
		Comment string `ch:"log_comment"`
	}
	if err := conn.Select(ctx, &comments, `
		SELECT DISTINCT log_comment FROM system.query_log
		WHERE event_date >= yesterday() AND log_comment LIKE 'chdbtest/%' AND type = 'QueryFinish'
		ORDER BY log_comment`); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"chdbtest/outer.inner": false, "chdbtest/other": false}
	for _, c := range comments {
		if _, ok := want[c.Comment]; ok {
			want[c.Comment] = true
		}
	}
	for c, seen := range want {
		if !seen {
			t.Errorf("no query named %q in the query log (got %v)", c, comments)
		}
	}
}
