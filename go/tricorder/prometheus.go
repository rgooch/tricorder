package tricorder

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cloud-Foundations/tricorder/go/tricorder/messages"
	"github.com/Cloud-Foundations/tricorder/go/tricorder/types"
	"github.com/Cloud-Foundations/tricorder/go/tricorder/units"
)

const (
	prometheusContentType = "text/plain; version=0.0.4"
)

// promFamily tracks metadata for a single Prometheus metric family so that
// HELP/TYPE lines are only emitted once per family.
type promFamily struct {
	help          string
	mtype         string
	headerEmitted bool
}

// prometheusCollector implements metricsCollector and streams metrics in the
// Prometheus text exposition format (v0.0.4).
type prometheusCollector struct {
	w        io.Writer
	families map[string]*promFamily
}

func newPrometheusCollector(w io.Writer) *prometheusCollector {
	return &prometheusCollector{
		w:        w,
		families: make(map[string]*promFamily),
	}
}

func (c *prometheusCollector) Collect(m *metric, s *session) error {
	var promMetric messages.Metric
	m.InitPromMetric(s, &promMetric)
	// InitPromMetric uses the native Go kinds (e.g. GoTime, GoDuration) and units
	if err := c.emitMetric(&promMetric); err != nil {
		return err
	}
	return nil
}

func (c *prometheusCollector) emitMetric(m *messages.Metric) error {
	// Lists do not have a natural Prometheus representation without
	// additional schema; skip them.
	if m.Kind == types.List {
		return nil
	}

	switch m.Kind {
	case types.Dist:
		return c.emitHistogram(m)
	case types.String:
		return c.emitStringInfo(m)
	case types.Bool:
		return c.emitBoolInfo(m)
	case types.Time, types.GoTime:
		return c.emitTimeGauge(m)
	default:
		// Numeric scalars, including durations and time-based values which will
		// be normalized based on their declared units.
		return c.emitNumeric(m)
	}
}

func (c *prometheusCollector) ensureFamily(name, help, mtype string) *promFamily {
	fam, ok := c.families[name]
	if ok {
		return fam
	}
	fam = &promFamily{help: help, mtype: mtype}
	c.families[name] = fam
	return fam
}

func (c *prometheusCollector) emitFamilyHeader(name, help, mtype string) error {
	fam := c.ensureFamily(name, help, mtype)
	if fam.headerEmitted {
		return nil
	}
	fam.headerEmitted = true

	help = sanitizeHelp(help)
	if _, err := fmt.Fprintf(c.w, "# HELP %s %s\n", name, help); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "# TYPE %s %s\n", name, mtype); err != nil {
		return err
	}
	return nil
}

func sanitizeHelp(help string) string {
	// Prometheus HELP text should escape backslashes and newlines.
	help = strings.ReplaceAll(help, "\\", "\\\\")
	help = strings.ReplaceAll(help, "\n", "\\n")
	return help
}

// promBaseName converts a Tricorder metric path (e.g. "/proc/go/num-goroutines")
// into a Prometheus-safe base metric name (e.g. "proc_go_num_goroutines").
func promBaseName(path string) string {
	path = strings.TrimPrefix(path, "/")
	name := strings.ReplaceAll(path, "/", "_")
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func isTimeUnit(u units.Unit) bool {
	switch u {
	case units.Millisecond, units.Second:
		return true
	default:
		return false
	}
}

func isByteUnit(u units.Unit) bool {
	switch u {
	case units.Byte, units.BytePerSecond:
		return true
	default:
		return false
	}
}

// durationToSeconds normalizes a duration value expressed in the given unit
// into seconds.
func durationToSeconds(value float64, u units.Unit) float64 {
	if !isTimeUnit(u) {
		return value
	}
	factor := units.FromSeconds(u)
	if factor == 0 {
		return value
	}
	return value / factor
}

// promNumericName returns the Prometheus metric name for a numeric metric,
// including unit-based suffixes like _seconds, _bytes, or _celsius.
//
// The choice of suffix also encodes whether the metric is treated as a
// Prometheus counter (…_total / …_seconds_total) or gauge. Most metrics follow
// a simple convention where a "-total" suffix in the Tricorder path maps to a
// Prometheus counter, but a few well-known metrics require semantic
// overrides:
//
//   - Memory usage/capacity metrics such as /sys/memory/total and
//     /proc/memory/total are gauges, even though their names include
//     "total". These are exported as *_bytes, not *_bytes_total.
//   - Network device statistics under /sys/netif (packets, bytes, errors,
//     drops, etc.) are monotonic counters and are exported as *_total
//     (e.g. sys_netif_eth0_rx_packets_total, sys_netif_eth0_rx_data_bytes_total),
//     similar to Prometheus node_exporter.
func promNumericName(m *messages.Metric) string {
	base := promBaseName(m.Path)
	// Duration kind is always logically "seconds with 9 decimal places" in
	// JSON, but the unit tells us whether it came from milliseconds or seconds.
	hasTotalSuffix := strings.HasSuffix(base, "_total")
	isMemGauge := isMemoryGaugeMetric(m)
	isNetCounter := isNetworkCounterMetric(m)
	isAdditionalCounter := isAdditionalCounterMetric(m)
	isExplicitCounter := hasTotalSuffix || isNetCounter || isAdditionalCounter
	if isMemGauge {
		// Never treat memory capacity/usage metrics as counters solely due to a
		// "-total" suffix in their names.
		hasTotalSuffix = false
	}
	if m.Kind == types.Duration || isTimeUnit(m.Unit) {
		// Durations and other time-like numeric scalars:
		//   - <base>_seconds       → gauge semantics.
		//   - <base>_seconds_total → counter semantics for explicit totals.
		if isExplicitCounter {
			if hasTotalSuffix {
				base = strings.TrimSuffix(base, "_total")
			}
			return base + "_seconds_total"
		}
		return base + "_seconds"
	}
	if isByteUnit(m.Unit) {
		// Sizes normalized to bytes:
		//   - <base>_bytes       → gauge semantics.
		//   - <base>_bytes_total → counter semantics.
		if isMemGauge {
			if hasTotalSuffix {
				base = strings.TrimSuffix(base, "_total")
			}
			return base + "_bytes"
		}
		if isExplicitCounter {
			if hasTotalSuffix {
				base = strings.TrimSuffix(base, "_total")
			}
			return base + "_bytes_total"
		}
		return base + "_bytes"
	}
	if m.Unit == units.Celsius {
		return base + "_celsius"
	}
	// Dimensionless scalars: use _total only for explicit totals or known
	// counter-style metrics such as /sys/netif device statistics and a small
	// set of additional event-style metrics.
	if isExplicitCounter {
		if hasTotalSuffix {
			base = strings.TrimSuffix(base, "_total")
		}
		return base + "_total"
	}
	return base
}

// isMemoryGaugeMetric identifies memory metrics that represent capacities or
// point-in-time usage and should be exported as gauges, despite having names
// that include "total".
//
// Examples from the JSON fixtures:
//   - /proc/memory/total: "System memory currently allocated to process".
//   - /sys/memory/total:  "total memory" (system memory capacity).
func isMemoryGaugeMetric(m *messages.Metric) bool {
	if m == nil {
		return false
	}
	if !isByteUnit(m.Unit) {
		return false
	}
	switch m.Path {
	case "/proc/memory/total", "/sys/memory/total":
		return true
	default:
		return false
	}
}

// isNetworkCounterMetric identifies network device statistics exported under
// /sys/netif that are naturally monotonic counters (packets, bytes, errors,
// drops, etc.). These should follow Prometheus counter naming conventions even
// though their base Tricorder names do not end in -total.
func isNetworkCounterMetric(m *messages.Metric) bool {
	if m == nil {
		return false
	}
	// Only numeric scalars participate in counter semantics.
	switch m.Kind {
	case types.Dist, types.String, types.Bool, types.Time, types.GoTime:
		return false
	}
	if !strings.HasPrefix(m.Path, "/sys/netif/") {
		return false
	}
	// Interface configuration values such as MTU and link speed are gauges.
	base := promBaseName(m.Path)
	if strings.HasSuffix(base, "_mtu") || strings.HasSuffix(base, "_speed") {
		return false
	}
	return true
}

// isAdditionalCounterMetric identifies a small set of non-network metrics that
// are semantically counters even though they may not carry an explicit
// "-total" suffix in their Tricorder path. These metrics are exported with
// Prometheus counter naming conventions (…_total / …_seconds_total).
func isAdditionalCounterMetric(m *messages.Metric) bool {
	if m == nil {
		return false
	}
	// Only numeric scalars participate in counter semantics.
	switch m.Kind {
	case types.Dist, types.String, types.Bool, types.Time, types.GoTime:
		return false
	}
	switch m.Path {
	// Event-style process lifecycle metrics.
	case "/proc/events/graceful-exits", "/proc/events/ungraceful-exits", "/proc/events/uncaught-panics":
		return true
	default:
		return false
	}
}

func promMetricType(name string, kind types.Type) string {
	if kind == types.Dist {
		return "histogram"
	}
	// Non-numeric kinds (string, bool, time) are always exported as
	// info-style gauges.
	switch kind {
	case types.String, types.Bool, types.Time, types.GoTime:
		return "gauge"
	}
	// Numeric metrics: suffix-based typing.
	if strings.HasSuffix(name, "_total") || strings.HasSuffix(name, "_seconds_total") {
		return "counter"
	}
	return "gauge"
}

// ClassifyPrometheusMetric derives the Prometheus metric name and type for a
// given Tricorder metric, mirroring the logic used by prometheusCollector.
//
// It returns exported=false for metric kinds that are not currently exported to
// Prometheus (for example, lists).
func ClassifyPrometheusMetric(m *messages.Metric) (name, mtype string, exported bool) {
	if m == nil {
		return "", "", false
	}
	// Lists do not have a natural Prometheus representation without additional
	// schema; skip them, as emitMetric does.
	if m.Kind == types.List {
		return "", "", false
	}
	// Match prometheusCollector.emitMetric and related helpers.
	switch m.Kind {
	case types.Dist:
		base := promBaseName(m.Path)
		if isTimeUnit(m.Unit) {
			base += "_seconds"
		} else if isByteUnit(m.Unit) {
			base += "_bytes"
		}
		return base, "histogram", true
	case types.String:
		return promBaseName(m.Path) + "_info", "gauge", true
	case types.Bool:
		return promBaseName(m.Path) + "_bool_info", "gauge", true
	case types.Time, types.GoTime:
		return promBaseName(m.Path) + "_time_seconds", "gauge", true
	default:
		name = promNumericName(m)
		mtype = promMetricType(name, m.Kind)
		return name, mtype, true
	}
}

// PrometheusNumericValue returns the numeric value for a metric in the same
// normalized units that Prometheus export uses. For time-based values this
// means seconds; for other numeric scalars it is the raw value converted to a
// float64.
//
// It returns ok=false if the metric cannot be represented as a single
// float64 (for example, distributions or non-numeric kinds).
func PrometheusNumericValue(m *messages.Metric) (value float64, ok bool) {
	if m == nil {
		return 0, false
	}
	value, ok = numericValue(m)
	if !ok {
		return 0, false
	}
	// Normalize durations/time-based values to seconds where applicable.
	if m.Kind == types.Duration || isTimeUnit(m.Unit) {
		value = durationToSeconds(value, m.Unit)
	}
	return value, true
}

func numericValue(m *messages.Metric) (float64, bool) {
	switch v := m.Value.(type) {
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case time.Duration:
		// Native Go duration value; convert into the metric's declared unit
		// so that durationToSeconds() can normalize back to seconds in the
		// same way it does for JSON string encodings.
		seconds := float64(v) / float64(time.Second)
		factor := units.FromSeconds(m.Unit)
		return seconds * factor, true
	case string:
		// JSON representation for duration metrics is a string seconds value.
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func (c *prometheusCollector) emitNumeric(m *messages.Metric) error {
	name := promNumericName(m)
	value, ok := numericValue(m)
	if !ok {
		// Skip metrics we cannot interpret as a single float64.
		return nil
	}
	// Normalize durations/time-based values to seconds where applicable.
	if m.Kind == types.Duration || isTimeUnit(m.Unit) {
		value = durationToSeconds(value, m.Unit)
	}
	mtype := promMetricType(name, m.Kind)
	if err := c.emitFamilyHeader(name, m.Description, mtype); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "%s %s\n", name, strconv.FormatFloat(value, 'g', -1, 64)); err != nil {
		return err
	}
	return nil
}

func (c *prometheusCollector) emitStringInfo(m *messages.Metric) error {
	value, ok := m.Value.(string)
	if !ok {
		return nil
	}
	name := promBaseName(m.Path) + "_info"
	mtype := "gauge"
	if err := c.emitFamilyHeader(name, m.Description, mtype); err != nil {
		return err
	}
	labelValue := escapeLabelValue(value)
	if _, err := fmt.Fprintf(c.w, "%s{value=\"%s\"} 1\n", name, labelValue); err != nil {
		return err
	}
	return nil
}

func (c *prometheusCollector) emitBoolInfo(m *messages.Metric) error {
	value, ok := m.Value.(bool)
	if !ok {
		return nil
	}
	name := promBaseName(m.Path) + "_bool_info"
	mtype := "gauge"
	if err := c.emitFamilyHeader(name, m.Description, mtype); err != nil {
		return err
	}
	valStr := "false"
	if value {
		valStr = "true"
	}
	if _, err := fmt.Fprintf(c.w, "%s{value=\"%s\"} 1\n", name, valStr); err != nil {
		return err
	}
	return nil
}

func (c *prometheusCollector) emitTimeGauge(m *messages.Metric) error {
	name := promBaseName(m.Path) + "_time_seconds"
	var seconds float64
	switch v := m.Value.(type) {
	case string:
		// JSON representation: already a stringified seconds value.
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		seconds = f
	case time.Time:
		// Native Go representation: convert to seconds since the epoch.
		seconds = float64(v.Unix()) + float64(v.Nanosecond())/1e9
	default:
		// Unknown encoding; ignore.
		return nil
	}
	mtype := "gauge"
	if err := c.emitFamilyHeader(name, m.Description, mtype); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "%s %s\n", name, strconv.FormatFloat(seconds, 'g', -1, 64)); err != nil {
		return err
	}
	return nil
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\n", "\\n")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	return v
}

func (c *prometheusCollector) emitHistogram(m *messages.Metric) error {
	dist, ok := m.Value.(*messages.Distribution)
	if !ok || dist == nil {
		return nil
	}
	base := promBaseName(m.Path)
	// Time-based distributions are normalized to seconds and get a _seconds
	// suffix; byte-based distributions would get _bytes.
	if isTimeUnit(m.Unit) {
		base = base + "_seconds"
	} else if isByteUnit(m.Unit) {
		base = base + "_bytes"
	}
	mtype := "histogram"
	if err := c.emitFamilyHeader(base, m.Description, mtype); err != nil {
		return err
	}

	// Prepare buckets: convert ranges to cumulative counts with normalized
	// upper bounds. Ranges may not be sorted, so sort defensively.
	ranges := make([]*messages.RangeWithCount, 0, len(dist.Ranges))
	for _, r := range dist.Ranges {
		if r == nil {
			continue
		}
		ranges = append(ranges, r)
	}
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Upper < ranges[j].Upper
	})

	var cumulative uint64
	for _, r := range ranges {
		cumulative += r.Count
		var le string
		if r.Upper == 0 {
			le = "+Inf"
		} else {
			upper := r.Upper
			if isTimeUnit(m.Unit) {
				upper = durationToSeconds(upper, m.Unit)
			}
			le = strconv.FormatFloat(upper, 'g', -1, 64)
		}
		if _, err := fmt.Fprintf(
			c.w,
			"%s_bucket{le=\"%s\"} %d\n",
			base,
			le,
			cumulative); err != nil {
			return err
		}
	}

	// Sum and count, normalizing sum for time-based units.
	sum := dist.Sum
	if isTimeUnit(m.Unit) {
		sum = durationToSeconds(sum, m.Unit)
	}
	if _, err := fmt.Fprintf(
		c.w,
		"%s_sum %s\n",
		base,
		strconv.FormatFloat(sum, 'g', -1, 64)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "%s_count %d\n", base, dist.Count); err != nil {
		return err
	}
	return nil
}

// writePrometheusMetrics writes all metrics from the global tricorder root in
// Prometheus text format to w.
func writePrometheusMetrics(w io.Writer) error {
	collector := newPrometheusCollector(w)
	return root.GetAllMetrics(collector, nil)
}

// prometheusHandlerFunc is the HTTP handler for /prometheus-metrics.
func prometheusHandlerFunc(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", prometheusContentType)
	if err := writePrometheusMetrics(w); err != nil {
		handleError(w, err)
	}
}

func initPrometheusHandlers() {
	// Use a trailing slash pattern so that requests to "/prometheus-metrics"
	// are automatically redirected to "/prometheus-metrics/", matching the
	// behavior of the HTML (/metrics) and JSON (/metricsapi) endpoints.
	http.Handle("/prometheus-metrics/", gzipHandler{http.HandlerFunc(prometheusHandlerFunc)})
}
