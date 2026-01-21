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
	prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"
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
	switch m.Kind {
	case types.List:
		// Lists do not have a natural Prometheus representation without
		// additional schema; skip them.
		return nil
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
	var b strings.Builder
	b.Grow(len(path))
	for i, r := range path {
		if i == 0 && r == '/' {
			// Skip leading slash
			continue
		}
		if r == '/' {
			b.WriteRune('_')
		} else if r >= 'A' && r <= 'Z' {
			// Convert uppercase to lowercase
			b.WriteRune(r + 32)
		} else if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			// Replace invalid characters with underscore
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
// including unit-based suffixes like _bytes or _celsius.
//
// All metrics are treated as gauges. Users should apply rate() or increase()
// functions in their queries for counter-like semantics.
func promNumericName(m *messages.Metric) string {
	base := promBaseName(m.Path)

	if isByteUnit(m.Unit) {
		// Byte-based metrics get _bytes suffix
		return base + "_bytes"
	}
	if m.Unit == units.Celsius {
		return base + "_celsius"
	}
	// Time-based and dimensionless scalars use base name as-is
	return base
}

func promMetricType(name string, kind types.Type) string {
	if kind == types.Dist {
		return "histogram"
	}
	// All other metrics are treated as gauges for simplicity and deterministic
	// behavior. Users can apply rate() or increase() functions in their queries
	// for counter-like semantics. Future enhancement: allow explicit counter
	// registration in Tricorder API.
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
	// Match prometheusCollector.emitMetric and related helpers.
	switch m.Kind {
	case types.List:
		// Lists do not have a natural Prometheus representation without additional
		// schema; skip them, as emitMetric does.
		return "", "", false
	case types.Dist:
		base := promBaseName(m.Path)
		if isByteUnit(m.Unit) {
			base += "_bytes"
		}
		return base, "histogram", true
	case types.String:
		return promBaseName(m.Path) + "_info", "gauge", true
	case types.Bool:
		return promBaseName(m.Path), "gauge", true
	case types.Time, types.GoTime:
		return promBaseName(m.Path), "gauge", true
	default:
		name = promNumericName(m)
		mtype = promMetricType(name, m.Kind)
		return name, mtype, true
	}
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
	// Format float using 'g' format (compact representation) with full precision (-1).
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
	name := promBaseName(m.Path)
	mtype := "gauge"
	if err := c.emitFamilyHeader(name, m.Description, mtype); err != nil {
		return err
	}
	var numericValue int
	if value {
		numericValue = 1
	}
	if _, err := fmt.Fprintf(c.w, "%s %d\n", name, numericValue); err != nil {
		return err
	}
	return nil
}

func (c *prometheusCollector) emitTimeGauge(m *messages.Metric) error {
	name := promBaseName(m.Path)
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
		seconds = float64(v.UnixNano()) / 1e9
	default:
		// Unknown encoding; ignore.
		return nil
	}
	mtype := "gauge"
	if err := c.emitFamilyHeader(name, m.Description, mtype); err != nil {
		return err
	}
	// Format float using 'g' format (compact representation) with full precision (-1).
	if _, err := fmt.Fprintf(c.w, "%s %s\n", name, strconv.FormatFloat(seconds, 'g', -1, 64)); err != nil {
		return err
	}
	return nil
}

func escapeLabelValue(v string) string {
	var b strings.Builder
	var needsEscape bool

	// Check if escaping is needed
	for _, r := range v {
		if r == '\\' || r == '\n' || r == '"' {
			needsEscape = true
			break
		}
	}

	// If no escaping needed, return original string
	if !needsEscape {
		return v
	}

	// Lazy allocation: only allocate builder if needed
	b.Grow(len(v))
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '"':
			b.WriteString("\\\"")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *prometheusCollector) emitHistogram(m *messages.Metric) error {
	dist, ok := m.Value.(*messages.Distribution)
	if !ok || dist == nil {
		return nil
	}
	base := promBaseName(m.Path)
	// Byte-based distributions get _bytes suffix.
	if isByteUnit(m.Unit) {
		base = base + "_bytes"
	}
	mtype := "histogram"
	if err := c.emitFamilyHeader(base, m.Description, mtype); err != nil {
		return err
	}

	// Prepare buckets: separate finite buckets from +Inf bucket (Upper == 0).
	// Sort finite buckets by upper bound, then emit +Inf last.
	var finiteBuckets []*messages.RangeWithCount
	var infBucketCount uint64
	var hasInfBucket bool
	for _, r := range dist.Ranges {
		if r == nil {
			continue
		}
		if r.Upper == 0 {
			// Upper == 0 represents +Inf in tricorder
			infBucketCount = r.Count
			hasInfBucket = true
		} else {
			finiteBuckets = append(finiteBuckets, r)
		}
	}
	sort.Slice(finiteBuckets, func(i, j int) bool {
		return finiteBuckets[i].Upper < finiteBuckets[j].Upper
	})

	// Emit finite buckets with cumulative counts
	var cumulative uint64
	for _, r := range finiteBuckets {
		cumulative += r.Count
		upper := r.Upper
		if isTimeUnit(m.Unit) {
			upper = durationToSeconds(upper, m.Unit)
		}
		le := strconv.FormatFloat(upper, 'g', -1, 64)
		if _, err := fmt.Fprint(c.w, base, "_bucket{le=\"", le, "\"} ", cumulative, "\n"); err != nil {
			return err
		}
	}

	// Always emit +Inf bucket last. Use dist.Count as total for +Inf.
	// If there was an explicit +Inf bucket, add its count to cumulative.
	if hasInfBucket {
		cumulative += infBucketCount
	}
	if _, err := fmt.Fprint(c.w, base, "_bucket{le=\"+Inf\"} ", dist.Count, "\n"); err != nil {
		return err
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
