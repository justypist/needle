package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"needle/internal/proto"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func report(id string, ts int64, cpu float64, series ...proto.Series) proto.Report {
	return proto.Report{
		Host:   proto.Host{ID: id, Name: id, Cores: 2},
		Sample: proto.Sample{TS: ts, CPU: cpu, MemTotal: 1000, MemUsed: 400, Load1: 0.5},
		Series: series,
	}
}

func TestIngestAndLatest(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()

	if err := st.Ingest(report("a", now-1000, 10, proto.Series{Kind: "cpu", Label: "cpu0", A: 10}), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := st.Ingest(report("a", now, 42, proto.Series{Kind: "cpu", Label: "cpu0", A: 42}), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	sample, series, ok, err := st.Latest("a")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if sample.CPU != 42 {
		t.Errorf("latest cpu = %v, want 42", sample.CPU)
	}
	if len(series) != 1 || series[0].A != 42 {
		t.Errorf("latest series = %+v, want one entry with a=42", series)
	}

	hosts, err := st.Hosts()
	if err != nil {
		t.Fatalf("hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].LastSeen != now {
		t.Errorf("hosts = %+v, want one host last seen at %d", hosts, now)
	}
}

func TestLatestOnUnknownHost(t *testing.T) {
	st := openTest(t)
	_, _, ok, err := st.Latest("nobody")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if ok {
		t.Fatal("unknown host reported as having data")
	}
}

func TestIngestRejectsMissingHostID(t *testing.T) {
	st := openTest(t)
	if err := st.Ingest(report("", time.Now().UnixMilli(), 1), 0); err == nil {
		t.Fatal("ingest accepted a report without a host id")
	}
}

// TestRollupAverages is the core invariant: a coarse bucket must equal the
// mean of the raw samples inside it, and counters must keep their last value
// rather than being averaged down.
func TestRollupAverages(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	width := Tiers[1].Width
	bucket := ((now - 2*time.Hour.Milliseconds()) / width) * width

	for i := 0; i < 10; i++ {
		ts := bucket + int64(i)*1000
		rep := report("a", ts, float64(i), proto.Series{Kind: "net", Label: "eth0", A: float64(i) * 100, B: float64(i)})
		rep.Sample.NetRxTot = uint64(1000 + i) // rises monotonically
		if err := st.Ingest(rep, 0); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	if err := st.Rollup(now); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	from, to := bucket, bucket+width
	got, err := st.Query("a", from, to+3*86400e3) // span forces tier 1
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Tier != 1 {
		t.Fatalf("tier = %d, want 1", got.Tier)
	}

	idx := -1
	for i, ts := range got.Ts {
		if ts == bucket {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("bucket %d missing from %v", bucket, got.Ts)
	}
	if v := got.Cols["cpu"][idx]; math.Abs(v-4.5) > 1e-9 {
		t.Errorf("rolled up cpu = %v, want 4.5", v)
	}
	if v := got.Cols["net_rx_total"][idx]; v != 1009 {
		t.Errorf("rolled up net_rx_total = %v, want the last value 1009", v)
	}
	if v := got.Cols["net:eth0"][idx]; math.Abs(v-450) > 1e-9 {
		t.Errorf("rolled up series a = %v, want 450", v)
	}
	if v := got.Cols["net:eth0:b"][idx]; math.Abs(v-4.5) > 1e-9 {
		t.Errorf("rolled up series b = %v, want 4.5", v)
	}
}

// TestRollupIgnoresOpenBucket guards against a rollup that publishes a
// half-filled bucket, which would make the chart dip on every refresh.
func TestRollupIgnoresOpenBucket(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	width := Tiers[1].Width

	if err := st.Ingest(report("a", now, 99), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := st.Rollup(now); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM samples WHERE tier = 1`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d tier-1 rows written for the still-open bucket", n)
	}
	_ = width
}

func TestRollupIsIdempotent(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	width := Tiers[1].Width
	bucket := ((now - 2*time.Hour.Milliseconds()) / width) * width

	for i := 0; i < 4; i++ {
		if err := st.Ingest(report("a", bucket+int64(i)*1000, float64(i*10)), 0); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	if err := st.Rollup(now); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	first := rollupCPU(t, st, "a", 1, bucket)
	if err := st.Rollup(now); err != nil {
		t.Fatalf("second rollup: %v", err)
	}
	if second := rollupCPU(t, st, "a", 1, bucket); second != first {
		t.Fatalf("rollup is not idempotent: %v then %v", first, second)
	}
	if first != 15 {
		t.Errorf("cpu = %v, want 15", first)
	}
}

func rollupCPU(t *testing.T, st *Store, host string, tier int, ts int64) float64 {
	t.Helper()
	var v float64
	if err := st.db.QueryRow(`SELECT cpu FROM samples WHERE host_id = ? AND tier = ? AND ts = ?`, host, tier, ts).Scan(&v); err != nil {
		t.Fatalf("read rolled up row: %v", err)
	}
	return v
}

func TestPruneDropsExpiredTiers(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()

	old := now - Tiers[0].Keep.Milliseconds() - time.Hour.Milliseconds()
	if err := st.Ingest(report("a", old, 5), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := st.Ingest(report("a", now, 7), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := st.Prune(now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM samples`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("kept %d raw rows, want 1", n)
	}
}

func TestQueryTierSelection(t *testing.T) {
	cases := []struct {
		span time.Duration
		want int
	}{
		{time.Hour, 0},
		{24 * time.Hour, 0},
		{3 * 24 * time.Hour, 1},
		{30 * 24 * time.Hour, 2},
	}
	for _, c := range cases {
		if got := pickTier(c.span.Milliseconds()); got != c.want {
			t.Errorf("pickTier(%v) = %d, want %d", c.span, got, c.want)
		}
	}
}

func TestQueryUnknownHost(t *testing.T) {
	st := openTest(t)
	if _, err := st.Query("nobody", 0, time.Now().UnixMilli()); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestQueryFillsGaps checks that a series which only exists for part of the
// window still produces a column aligned with the time axis.
func TestQueryFillsGaps(t *testing.T) {
	st := openTest(t)
	// Fixed timestamps keep this on the raw tier regardless of when it runs.
	now := int64(1_700_000_000_000)

	if err := st.Ingest(report("a", now-3000, 1), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := st.Ingest(report("a", now-1000, 2, proto.Series{Kind: "cpu", Label: "cpu0", A: 2}), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got, err := st.Query("a", now-10000, now)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got.Ts) != 2 {
		t.Fatalf("points = %d, want 2", len(got.Ts))
	}
	col := got.Cols["cpu:cpu0"]
	if len(col) != len(got.Ts) {
		t.Fatalf("column length %d != axis length %d", len(col), len(got.Ts))
	}
	if !math.IsNaN(col[0]) {
		t.Errorf("gap at index 0 = %v, want NaN", col[0])
	}
	if col[1] != 2 {
		t.Errorf("value at index 1 = %v, want 2", col[1])
	}
	if len(got.Cols["cpu"]) != 2 {
		t.Errorf("scalar column has %d entries, want 2", len(got.Cols["cpu"]))
	}
}

func TestDeleteHost(t *testing.T) {
	st := openTest(t)
	now := time.Now().UnixMilli()
	if err := st.Ingest(report("a", now, 1, proto.Series{Kind: "cpu", Label: "cpu0", A: 1}), 0); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := st.DeleteHost("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	stats, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	for _, key := range []string{"hosts", "samples", "series"} {
		if stats[key] != 0 {
			t.Errorf("%s = %d after delete, want 0", key, stats[key])
		}
	}
}

func TestF64MarshalsGapsAsNull(t *testing.T) {
	got, err := F64{1, math.NaN(), 3}.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != "[1,null,3]" {
		t.Errorf("got %s, want [1,null,3]", got)
	}
}

func TestParseRange(t *testing.T) {
	if got := ParseRange("", time.Hour, maxTestRange); got != time.Hour {
		t.Errorf("empty = %v, want the default", got)
	}
	if got := ParseRange("12h", time.Hour, maxTestRange); got != 12*time.Hour {
		t.Errorf("12h = %v", got)
	}
	if got := ParseRange("900d", time.Hour, maxTestRange); got != maxTestRange {
		t.Errorf("oversized = %v, want the maximum", got)
	}
	if got := ParseRange("nonsense", time.Hour, maxTestRange); got != time.Hour {
		t.Errorf("invalid = %v, want the default", got)
	}
}

const maxTestRange = 365 * 24 * time.Hour
