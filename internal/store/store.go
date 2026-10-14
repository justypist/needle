// Package store persists host metadata and metric history in SQLite.
//
// History is kept in three tiers. Tier 0 is what agents send; tier 1 and
// tier 2 are rolling averages rolled up from the tier below. Every metric,
// including byte counters that agents already converted to per-second rates,
// is a gauge, so collapsing a tier is a plain average — which keeps the
// rollup queries uniform.
package store

import (
	"bytes"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"needle/internal/proto"

	_ "modernc.org/sqlite"
)

// Tiers are ordered from finest to coarsest. Width is the bucket size in
// milliseconds and Keep is how long a tier is retained.
var Tiers = []struct {
	Width int64
	Keep  time.Duration
}{
	{Width: 0, Keep: 48 * time.Hour},
	{Width: 5 * 60 * 1000, Keep: 30 * 24 * time.Hour},
	{Width: 60 * 60 * 1000, Keep: 365 * 24 * time.Hour},
}

// maxRows bounds how many samples a single chart query pulls; when a range
// holds more than this the query is strided and uPlot draws what it gets.
const maxRows = 3000

const sampleCols = `cpu, mem_total, mem_used, mem_cached, swap_total, swap_used,
	load1, load5, load15, uptime, procs,
	net_rx, net_tx, net_rx_total, net_tx_total,
	disk_read, disk_write, disk_read_total, disk_write_total, temp`

var sampleColNames = []string{
	"cpu", "mem_total", "mem_used", "mem_cached", "swap_total", "swap_used",
	"load1", "load5", "load15", "uptime", "procs",
	"net_rx", "net_tx", "net_rx_total", "net_tx_total",
	"disk_read", "disk_write", "disk_read_total", "disk_write_total", "temp",
}

// ErrNotFound is returned when a host id is unknown.
var ErrNotFound = errors.New("not found")

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// HostRow is the stored identity of a machine plus its report bookends.
type HostRow struct {
	proto.Host
	FirstSeen int64 `json:"first_seen"`
	LastSeen  int64 `json:"last_seen"`
}

// Range is one chart query result: a shared time axis plus named columns.
type Range struct {
	Host HostRow        `json:"host"`
	Tier int            `json:"tier"`
	Step int64          `json:"step"`
	From int64          `json:"from"`
	To   int64          `json:"to"`
	Ts   []int64        `json:"ts"`
	Cols map[string]F64 `json:"cols"`
}

// F64 marshals NaN as null so that gaps in a series survive the trip to the
// browser, where uPlot renders them as breaks rather than as zeroes.
type F64 []float64

func (f F64) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, v := range f {
		if i > 0 {
			b.WriteByte(',')
		}
		if math.IsNaN(v) {
			b.WriteString("null")
			continue
		}
		b.Write(strconv.AppendFloat(nil, v, 'g', -1, 64))
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// Open opens the database and applies the schema, creating the file if
// needed. WAL keeps readers (dashboard) from blocking the writer (ingest).
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serialises writers anyway; a single connection avoids
	// SQLITE_BUSY between the ingest and rollup paths.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS hosts (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  os         TEXT NOT NULL DEFAULT '',
  kernel     TEXT NOT NULL DEFAULT '',
  arch       TEXT NOT NULL DEFAULT '',
  cpu_model  TEXT NOT NULL DEFAULT '',
  cores      INTEGER NOT NULL DEFAULT 0,
  boot_time  INTEGER NOT NULL DEFAULT 0,
  agent      TEXT NOT NULL DEFAULT '',
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS samples (
  host_id TEXT NOT NULL,
  tier    INTEGER NOT NULL,
  ts      INTEGER NOT NULL,
  ` + sampleCols + `,
  PRIMARY KEY (host_id, tier, ts)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS series (
  host_id TEXT NOT NULL,
  tier    INTEGER NOT NULL,
  ts      INTEGER NOT NULL,
  kind    TEXT NOT NULL,
  label   TEXT NOT NULL,
  a       REAL NOT NULL DEFAULT 0,
  b       REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (host_id, tier, ts, kind, label)
) WITHOUT ROWID;
`)
	return err
}

// Ingest writes one report and refreshes the host's metadata.
func (s *Store) Ingest(r proto.Report, interval int) error {
	if r.Host.ID == "" {
		return errors.New("report has no host id")
	}
	ts := r.Sample.TS
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
INSERT INTO hosts (id, name, os, kernel, arch, cpu_model, cores, boot_time, agent, first_seen, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name=excluded.name, os=excluded.os, kernel=excluded.kernel, arch=excluded.arch,
  cpu_model=excluded.cpu_model, cores=excluded.cores, boot_time=excluded.boot_time,
  agent=excluded.agent, last_seen=excluded.last_seen`,
		r.Host.ID, r.Host.Name, r.Host.OS, r.Host.Kernel, r.Host.Arch,
		r.Host.CPUModel, r.Host.Cores, r.Host.BootTime, r.Host.Agent, ts, ts)
	if err != nil {
		return err
	}

	sample := r.Sample
	sample.TS = ts
	args := []any{r.Host.ID, 0, ts}
	args = append(args, sampleValues(&sample)...)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO samples (host_id, tier, ts, `+sampleCols+`)
VALUES (?, ?, ?, `+placeholders(len(sampleColNames))+`)`, args...); err != nil {
		return err
	}

	if len(r.Series) > 0 {
		stmt, err := tx.Prepare(`INSERT OR REPLACE INTO series (host_id, tier, ts, kind, label, a, b)
VALUES (?, 0, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, ser := range r.Series {
			if ser.Label == "" {
				continue
			}
			if _, err := stmt.Exec(r.Host.ID, ts, ser.Kind, ser.Label, ser.A, ser.B); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func sampleValues(s *proto.Sample) []any {
	var temp any
	if s.Temp != nil {
		temp = *s.Temp
	}
	return []any{
		s.CPU, s.MemTotal, s.MemUsed, s.MemCached, s.SwapTotal, s.SwapUsed,
		s.Load1, s.Load5, s.Load15, s.Uptime, s.Procs,
		s.NetRx, s.NetTx, s.NetRxTot, s.NetTxTot,
		s.DiskRead, s.DiskWrite, s.DiskRTot, s.DiskWTot, temp,
	}
}

func placeholders(n int) string {
	b := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

// Hosts returns every known host, most recently seen first.
func (s *Store) Hosts() ([]HostRow, error) {
	rows, err := s.db.Query(`
SELECT id, name, os, kernel, arch, cpu_model, cores, boot_time, agent, first_seen, last_seen
FROM hosts ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostRow
	for rows.Next() {
		var h HostRow
		if err := rows.Scan(&h.ID, &h.Name, &h.OS, &h.Kernel, &h.Arch, &h.CPUModel,
			&h.Cores, &h.BootTime, &h.Agent, &h.FirstSeen, &h.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Host returns one host's metadata.
func (s *Store) Host(id string) (HostRow, error) {
	var h HostRow
	err := s.db.QueryRow(`
SELECT id, name, os, kernel, arch, cpu_model, cores, boot_time, agent, first_seen, last_seen
FROM hosts WHERE id = ?`, id).
		Scan(&h.ID, &h.Name, &h.OS, &h.Kernel, &h.Arch, &h.CPUModel,
			&h.Cores, &h.BootTime, &h.Agent, &h.FirstSeen, &h.LastSeen)
	return h, err
}

// Latest returns the most recent raw sample and series for a host. The bool
// is false when the host has never reported.
func (s *Store) Latest(id string) (proto.Sample, []proto.Series, bool, error) {
	row := s.db.QueryRow(`SELECT ts, `+sampleCols+` FROM samples
WHERE host_id = ? AND tier = 0 ORDER BY ts DESC LIMIT 1`, id)

	var sample proto.Sample
	var temp sql.NullFloat64
	dest := sampleScanDest(&sample, &temp)
	dest = append([]any{&sample.TS}, dest...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sample, nil, false, nil
		}
		return sample, nil, false, err
	}
	applyTemp(&sample, temp)

	rows, err := s.db.Query(`SELECT kind, label, a, b FROM series
WHERE host_id = ? AND tier = 0 AND ts = ?`, id, sample.TS)
	if err != nil {
		return sample, nil, true, err
	}
	defer rows.Close()

	var series []proto.Series
	for rows.Next() {
		var ser proto.Series
		if err := rows.Scan(&ser.Kind, &ser.Label, &ser.A, &ser.B); err != nil {
			return sample, nil, true, err
		}
		series = append(series, ser)
	}
	return sample, series, true, rows.Err()
}

func sampleScanDest(s *proto.Sample, temp *sql.NullFloat64) []any {
	return []any{
		&s.CPU, &s.MemTotal, &s.MemUsed, &s.MemCached, &s.SwapTotal, &s.SwapUsed,
		&s.Load1, &s.Load5, &s.Load15, &s.Uptime, &s.Procs,
		&s.NetRx, &s.NetTx, &s.NetRxTot, &s.NetTxTot,
		&s.DiskRead, &s.DiskWrite, &s.DiskRTot, &s.DiskWTot, temp,
	}
}

func applyTemp(s *proto.Sample, temp sql.NullFloat64) {
	if temp.Valid {
		v := temp.Float64
		s.Temp = &v
	}
}

// Query returns chart data for one host between from and to, inclusive, in
// milliseconds. Both the tier and the stride come from the span alone, so a
// shorter window automatically gives finer data.
func (s *Store) Query(id string, from, to int64) (Range, error) {
	host, err := s.Host(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Range{}, ErrNotFound
		}
		return Range{}, err
	}
	tier := pickTier(to - from)

	out := Range{
		Host: host,
		Tier: tier,
		From: from,
		To:   to,
		Ts:   []int64{},
		Cols: map[string]F64{},
	}
	if tier > 0 {
		out.Step = Tiers[tier].Width
	} else {
		out.Step = s.rawStep(id, from, to)
	}

	// Bucket rows are aligned to the epoch rather than to the window, so the
	// query starts at the first bucket at or after `from` and simply takes
	// the next maxRows of them. Raw rows sit wherever the agent's clock put
	// them, so that tier is limited by count the same way.
	if tier > 0 {
		from = ceilTo(from, out.Step)
	}
	rows, err := s.db.Query(`SELECT ts, `+sampleCols+` FROM samples
WHERE host_id = ? AND tier = ? AND ts >= ?
ORDER BY ts LIMIT ?`, id, tier, from, maxRows)
	if err != nil {
		return Range{}, err
	}
	defer rows.Close()

	pos := map[int64]int{}
	cols := make(map[string]F64, len(sampleColNames))
	for _, name := range sampleColNames {
		cols[name] = make(F64, 0, maxRows)
	}
	for rows.Next() {
		var ts int64
		sample := proto.Sample{}
		var temp sql.NullFloat64
		dest := append([]any{&ts}, sampleScanDest(&sample, &temp)...)
		if err := rows.Scan(dest...); err != nil {
			return Range{}, err
		}
		pos[ts] = len(out.Ts)
		out.Ts = append(out.Ts, ts)
		values := sampleValues(&sample)
		for i, name := range sampleColNames {
			cols[name] = append(cols[name], toFloat(values[i]))
		}
	}
	if err := rows.Err(); err != nil {
		return Range{}, err
	}
	if len(out.Ts) == 0 {
		return out, nil
	}

	srows, err := s.db.Query(`SELECT ts, kind, label, a, b FROM series
WHERE host_id = ? AND tier = ? AND ts >= ?
ORDER BY ts LIMIT ?`, id, tier, from, maxRows*16)
	if err != nil {
		return Range{}, err
	}
	defer srows.Close()

	n := len(out.Ts)
	// Columns are discovered as series rows arrive, so they start as all-gap
	// and are padded to the full axis at the end.
	acc := map[string]*colAcc{}
	column := func(key string) *colAcc {
		c, ok := acc[key]
		if !ok {
			c = &colAcc{a: make([]float64, n), real: map[int]bool{}}
			for i := range c.a {
				c.a[i] = math.NaN()
			}
			acc[key] = c
		}
		return c
	}
	for srows.Next() {
		var ts int64
		var ser proto.Series
		if err := srows.Scan(&ts, &ser.Kind, &ser.Label, &ser.A, &ser.B); err != nil {
			return Range{}, err
		}
		i, ok := pos[ts]
		if !ok {
			continue
		}
		keyA := ser.Kind + ":" + ser.Label
		ca := column(keyA)
		ca.a[i] = ser.A
		ca.real[i] = true

		if ser.Kind == "net" || ser.Kind == "disk" || ser.Kind == "fs" {
			cb := column(keyA + ":b")
			cb.a[i] = ser.B
			cb.real[i] = true
		}
	}
	if err := srows.Err(); err != nil {
		return Range{}, err
	}
	for key, c := range acc {
		if len(c.real) == 0 {
			continue
		}
		out.Cols[key] = c.a
	}
	for name, col := range cols {
		if len(col) > 0 {
			out.Cols[name] = col
		}
	}
	return out, nil
}

// colAcc accumulates one series column while its rows stream in.
type colAcc struct {
	a    []float64
	real map[int]bool
}

// pickRawStep infers the agent's reporting interval from recent raw rows so
// that the chart's step label reflects reality instead of a guess.
func (s *Store) rawStep(id string, from, to int64) int64 {
	const fallback = 30 * 1000
	var a, b int64
	err := s.db.QueryRow(`SELECT ts FROM samples WHERE host_id = ? AND tier = 0
AND ts >= ? AND ts <= ? ORDER BY ts LIMIT 1`, id, from, to).Scan(&a)
	if err != nil {
		return fallback
	}
	err = s.db.QueryRow(`SELECT ts FROM samples WHERE host_id = ? AND tier = 0
AND ts >= ? AND ts <= ? ORDER BY ts DESC LIMIT 1`, id, from, to).Scan(&b)
	if err != nil || b <= a {
		return fallback
	}
	var n int64
	if err := s.db.QueryRow(`SELECT count(*) FROM samples
WHERE host_id = ? AND tier = 0 AND ts >= ? AND ts <= ?`, id, a, b).Scan(&n); err != nil || n < 2 {
		return fallback
	}
	step := (b - a) / (n - 1)
	if step <= 0 {
		return fallback
	}
	return step
}

// pickTier chooses the finest tier whose row count for the span stays within
// maxRows, so the chart keeps as much resolution as the payload allows. The
// span thresholds below mirror the tier setup in Tiers.
func pickTier(span int64) int {
	const (
		fiveMin = 5 * 60 * 1000
		hour    = 60 * 60 * 1000
	)
	switch {
	case span <= maxRows*30*1000: // <= ~25h of raw
		return 0
	case span <= maxRows*fiveMin: // <= ~104d of 5-minute buckets
		return 1
	default:
		return 2
	}
}

// Rollup rebuilds the aggregate tiers from their source tier. It only
// touches buckets that have already closed, and recomputing a closed bucket
// from complete source data always yields the same value, so running it
// often is harmless.
func (s *Store) Rollup(now int64) error {
	for dest := 1; dest < len(Tiers); dest++ {
		if err := s.rollupTier(dest-1, dest, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) rollupTier(src, dest int, now int64) error {
	width := Tiers[dest].Width
	upper := (now / width) * width // start of the still-open bucket
	lower := lowerBound(dest, now)
	if upper <= lower {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
INSERT OR REPLACE INTO samples (host_id, tier, ts, `+sampleCols+`)
SELECT host_id, ?, (ts / ?) * ?, `+aggCols()+`
FROM samples WHERE tier = ? AND ts >= ? AND ts < ?
GROUP BY host_id, (ts / ?)`,
		dest, width, width, src, lower, upper, width)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`
INSERT OR REPLACE INTO series (host_id, tier, ts, kind, label, a, b)
SELECT host_id, ?, (ts / ?) * ?, kind, label, avg(a), avg(b)
FROM series WHERE tier = ? AND ts >= ? AND ts < ?
GROUP BY host_id, (ts / ?), kind, label`,
		dest, width, width, src, lower, upper, width)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// aggCols is the rollup projection: gauges are averaged, counters are
// maximised (they only rise, so the max is the end-of-bucket value), and
// capacities are maximised because they may change mid-bucket.
func aggCols() string {
	return `avg(cpu), max(mem_total), avg(mem_used), avg(mem_cached),
		max(swap_total), avg(swap_used),
		avg(load1), avg(load5), avg(load15),
		max(uptime), avg(procs),
		avg(net_rx), avg(net_tx), max(net_rx_total), max(net_tx_total),
		avg(disk_read), avg(disk_write), max(disk_read_total), max(disk_write_total),
		avg(temp)`
}

// lowerBound is how far back a rollup needs to look: the source tier's own
// retention window, since anything older has already been rolled up.
func lowerBound(dest int, now int64) int64 {
	return now - Tiers[dest-1].Keep.Milliseconds()
}

// Prune drops rows past each tier's retention.
func (s *Store) Prune(now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, t := range Tiers {
		cutoff := now - t.Keep.Milliseconds()
		if _, err := tx.Exec(`DELETE FROM samples WHERE tier = ? AND ts < ?`, i, cutoff); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM series WHERE tier = ? AND ts < ?`, i, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HostIDs returns every known host id, used by the maintenance loop.
func (s *Store) HostIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteHost removes a host and everything recorded for it.
func (s *Store) DeleteHost(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM series WHERE host_id = ?`,
		`DELETE FROM samples WHERE host_id = ?`,
		`DELETE FROM hosts WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Stats reports row counts, for the tiny /api/stats endpoint.
func (s *Store) Stats() (map[string]int64, error) {
	out := map[string]int64{}
	for name, q := range map[string]string{
		"hosts":   `SELECT count(*) FROM hosts`,
		"samples": `SELECT count(*) FROM samples`,
		"series":  `SELECT count(*) FROM series`,
	} {
		var n int64
		if err := s.db.QueryRow(q).Scan(&n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, nil
}

// toFloat converts a value straight out of sampleValues. The integer cases
// matter: byte counts and uptime are uint64, and missing them would turn
// every such column into a gap on the chart.
func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case nil:
		return math.NaN()
	default:
		return math.NaN()
	}
}

// String renders a range for logs.
func (r Range) String() string {
	return "host=" + r.Host.ID + " tier=" + strconv.Itoa(r.Tier) +
		" step=" + strconv.FormatInt(r.Step, 10) + "ms points=" + strconv.Itoa(len(r.Ts))
}

// ceilTo rounds ms up to the next multiple of step.
func ceilTo(ms, step int64) int64 {
	if step <= 0 {
		return ms
	}
	return ((ms + step - 1) / step) * step
}

// ParseRange resolves the ?range= duration accepted by the API. Go's own
// parser stops at hours, but a dashboard naturally asks for "7d" or "90d",
// so day and week suffixes are understood here as well.
func ParseRange(s string, def, max time.Duration) time.Duration {
	d, ok := parseDuration(s)
	if !ok || d <= 0 {
		return def
	}
	if d > max {
		return max
	}
	return d
}

func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, unit := range []struct {
		suffix string
		size   time.Duration
	}{
		{"d", 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
	} {
		n, found := strings.CutSuffix(s, unit.suffix)
		if !found {
			continue
		}
		// Only a bare number counts, so "1h30m" is not mistaken for days.
		v, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		return time.Duration(v * float64(unit.size)), true
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}
