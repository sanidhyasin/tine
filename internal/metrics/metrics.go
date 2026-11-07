// Package metrics is tined's Prometheus metrics: how long each stage of
// creating a VM takes, destroys, execs, Firecracker API calls, API requests,
// the reaper and reconcile, and how many VMs are in each state.
//
// Every method works on a nil *Metrics and does nothing, so code and tests
// that don't want metrics don't need any.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "tine"

// Buckets, in seconds. VM operations span 100µs (allocating an ID) to tens
// of seconds (a slow boot); Firecracker API calls are mostly well under a
// millisecond.
var (
	operationBuckets   = prometheus.ExponentialBuckets(0.0001, 2.5, 15)  // 100µs to about 37s
	firecrackerBuckets = prometheus.ExponentialBuckets(0.00005, 2.5, 12) // 50µs to about 0.7s
)

// Metrics holds tined's collectors and the registry they're in.
type Metrics struct {
	registry *prometheus.Registry

	createStage    *prometheus.HistogramVec
	creates        *prometheus.CounterVec
	createFailures *prometheus.CounterVec
	destroySeconds prometheus.Histogram
	destroys       *prometheus.CounterVec
	execSeconds    prometheus.Histogram
	execs          *prometheus.CounterVec
	fcSeconds      *prometheus.HistogramVec
	fcErrors       *prometheus.CounterVec
	apiRequests    *prometheus.CounterVec
	apiSeconds     *prometheus.HistogramVec
	reaped         prometheus.Counter
	reconciled     *prometheus.CounterVec
}

// New returns metrics in a registry of their own, along with the Go runtime
// and process metrics and tine_build_info.
func New(version string) *Metrics {
	reg := prometheus.NewRegistry()
	f := factory{reg}
	m := &Metrics{
		registry: reg,
		createStage: f.histogramVec("vm_create_stage_seconds",
			"How long each stage of creating a VM took: allocate, network, jail, start, configure, boot, and the total.",
			operationBuckets, "stage"),
		creates:        f.counterVec("vm_creates_total", "VM creates, by result (ok or failed).", "result"),
		createFailures: f.counterVec("vm_create_failures_total", "Failed VM creates, by the stage that failed.", "stage"),
		destroySeconds: f.histogram("vm_destroy_seconds", "How long destroying a VM took.", operationBuckets),
		destroys:       f.counterVec("vm_destroys_total", "VM destroys, by result (ok or failed).", "result"),
		execSeconds:    f.histogram("exec_seconds", "How long running a command in a VM took, end to end.", operationBuckets),
		execs:          f.counterVec("execs_total", "Commands run in VMs, by result: ok (it ran, whatever its exit code) or error.", "result"),
		fcSeconds: f.histogramVec("firecracker_api_seconds",
			"How long calls to Firecracker's API took, by endpoint.", firecrackerBuckets, "endpoint"),
		fcErrors:    f.counterVec("firecracker_api_errors_total", "Failed calls to Firecracker's API, by endpoint.", "endpoint"),
		apiRequests: f.counterVec("api_requests_total", "Requests to tined's API, by method, route and status code.", "method", "route", "code"),
		apiSeconds: f.histogramVec("api_request_seconds",
			"How long tined took to answer API requests, by method and route.", operationBuckets, "method", "route"),
		reaped:     f.counter("reaper_destroyed_total", "VMs the reaper destroyed because their time to live ran out."),
		reconciled: f.counterVec("reconcile_actions_total", "What reconciling at startup did, by action: kept, finished, cleaned, orphan.", "action"),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace, Name: "build_info", Help: "Always 1; the version label says which tined is running.",
			ConstLabels: prometheus.Labels{"version": version},
		}, func() float64 { return 1 }),
	)
	return m
}

// Handler serves the metrics in Prometheus's text format.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry is where the metrics live, for tests and extra collectors.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// WatchVMStates adds tine_vms{state}: how many VMs are in each of states,
// as count reports them when Prometheus scrapes.
func (m *Metrics) WatchVMStates(states []string, count func() (map[string]int, error)) {
	if m == nil {
		return
	}
	m.registry.MustRegister(&vmStates{
		desc:   prometheus.NewDesc(namespace+"_vms", "VMs that exist, by state.", []string{"state"}, nil),
		states: states,
		count:  count,
	})
}

// CreateSucceeded records a create and its stage timings, in milliseconds.
func (m *Metrics) CreateSucceeded(timingsMs map[string]float64) {
	if m == nil {
		return
	}
	m.creates.WithLabelValues("ok").Inc()
	for stage, ms := range timingsMs {
		if stage == "agent" {
			stage = "boot" // waiting for the agent is the guest booting
		}
		m.createStage.WithLabelValues(stage).Observe(ms / 1000)
	}
}

// CreateFailed records a create that failed at stage.
func (m *Metrics) CreateFailed(stage string) {
	if m == nil {
		return
	}
	m.creates.WithLabelValues("failed").Inc()
	m.createFailures.WithLabelValues(stage).Inc()
}

// Destroyed records a destroy.
func (m *Metrics) Destroyed(d time.Duration, err error) {
	if m == nil {
		return
	}
	m.destroys.WithLabelValues(result(err)).Inc()
	if err == nil {
		m.destroySeconds.Observe(d.Seconds())
	}
}

// Exec records a command run in a VM. A command that ran and exited nonzero
// isn't an error; only failing to run it or losing the connection is.
func (m *Metrics) Exec(d time.Duration, err error) {
	if m == nil {
		return
	}
	m.execs.WithLabelValues(result(err)).Inc()
	m.execSeconds.Observe(d.Seconds())
}

// FirecrackerCall records one call to a Firecracker API endpoint, such as
// "PUT /machine-config".
func (m *Metrics) FirecrackerCall(endpoint string, d time.Duration, err error) {
	if m == nil {
		return
	}
	m.fcSeconds.WithLabelValues(endpoint).Observe(d.Seconds())
	if err != nil {
		m.fcErrors.WithLabelValues(endpoint).Inc()
	}
}

// APIRequest records a request to tined's API. route is the matched
// pattern, such as "/v1/vms/{id}", so IDs don't become labels.
func (m *Metrics) APIRequest(method, route string, code int, d time.Duration) {
	if m == nil {
		return
	}
	m.apiRequests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	m.apiSeconds.WithLabelValues(method, route).Observe(d.Seconds())
}

// Reaped records VMs the reaper destroyed.
func (m *Metrics) Reaped(n int) {
	if m == nil || n == 0 {
		return
	}
	m.reaped.Add(float64(n))
}

// Reconciled records what reconciling did, n times action.
func (m *Metrics) Reconciled(action string, n int) {
	if m == nil || n == 0 {
		return
	}
	m.reconciled.WithLabelValues(action).Add(float64(n))
}

func result(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}

// vmStates reads VM counts when Prometheus scrapes, so they're never stale.
type vmStates struct {
	desc   *prometheus.Desc
	states []string
	count  func() (map[string]int, error)
}

func (v *vmStates) Describe(ch chan<- *prometheus.Desc) { ch <- v.desc }

func (v *vmStates) Collect(ch chan<- prometheus.Metric) {
	counts, err := v.count()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(v.desc, err)
		return
	}
	for _, s := range v.states {
		ch <- prometheus.MustNewConstMetric(v.desc, prometheus.GaugeValue, float64(counts[s]), s)
	}
}

// factory makes collectors in tine's namespace and registers them.
type factory struct{ reg *prometheus.Registry }

func (f factory) histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(h)
	return h
}

func (f factory) histogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets})
	f.reg.MustRegister(h)
	return h
}

func (f factory) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	f.reg.MustRegister(c)
	return c
}

func (f factory) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help})
	f.reg.MustRegister(c)
	return c
}
