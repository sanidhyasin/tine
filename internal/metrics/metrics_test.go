package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordsAndExposition(t *testing.T) {
	m := New("v-test")
	m.CreateSucceeded("cold", map[string]float64{"network": 5.1, "agent": 1800, "total": 1850})
	m.CreateSucceeded("snapshot", map[string]float64{"restore": 9.5, "agent": 2, "total": 40})
	m.CreateFailed("agent")
	m.Destroyed(30*time.Millisecond, nil)
	m.Destroyed(time.Second, errors.New("busy"))
	m.Exec(3*time.Millisecond, nil)
	m.FirecrackerCall("PUT /machine-config", 200*time.Microsecond, nil)
	m.FirecrackerCall("PUT /actions", time.Millisecond, errors.New("400"))
	m.APIRequest("POST", "/v1/vms", 201, 2*time.Second)
	m.Reaped(2)
	m.Reconciled("cleaned", 1)
	m.Reconciled("kept", 0)
	m.WatchVMStates([]string{"running", "failed"}, func() (map[string]int, error) {
		return map[string]int{"running": 3}, nil
	})

	checks := map[string]float64{
		`tine_vm_creates_total{result="ok"}`:                                2,
		`tine_vm_creates_total{result="failed"}`:                            1,
		`tine_vm_create_failures_total{stage="agent"}`:                      1,
		`tine_vm_destroys_total{result="failed"}`:                           1,
		`tine_execs_total{result="ok"}`:                                     1,
		`tine_firecracker_api_errors_total{endpoint="PUT /actions"}`:        1,
		`tine_api_requests_total{code="201",method="POST",route="/v1/vms"}`: 1,
		`tine_reaper_destroyed_total`:                                       2,
		`tine_reconcile_actions_total{action="cleaned"}`:                    1,
	}
	body := scrape(t, m)
	for series, want := range checks {
		if !strings.Contains(body, series+" "+formatFloat(want)) {
			t.Errorf("missing %s %v", series, want)
		}
	}
	for _, want := range []string{
		`tine_vm_create_stage_seconds_count{source="cold",stage="boot"} 1`, // "agent" is shown as "boot"
		`tine_vm_create_stage_seconds_count{source="cold",stage="network"} 1`,
		`tine_vm_create_stage_seconds_count{source="snapshot",stage="agent"} 1`, // but not after a restore
		`tine_vm_create_stage_seconds_count{source="snapshot",stage="restore"} 1`,
		`tine_vm_destroy_seconds_count 1`, // only successful destroys are timed
		`tine_vms{state="running"} 3`,
		`tine_vms{state="failed"} 0`, // every state appears, even at 0
		`tine_build_info{version="v-test"} 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
	if strings.Contains(body, `action="kept"`) {
		t.Error("a zero count was recorded as a series")
	}
}

func TestStageTimingsLandInTheRightBuckets(t *testing.T) {
	m := New("v")
	m.CreateSucceeded("cold", map[string]float64{"total": 1850})
	if got := testutil.CollectAndCount(m.createStage); got != 1 {
		t.Fatalf("%d series, want 1", got)
	}
	body := scrape(t, m)
	// 1.85s is above the 0.95s bucket and within the 2.4s one.
	for _, want := range []string{
		`tine_vm_create_stage_seconds_bucket{source="cold",stage="total",le="0.95367431640625"} 0`,
		`tine_vm_create_stage_seconds_bucket{source="cold",stage="total",le="2.384185791015625"} 1`,
		`tine_vm_create_stage_seconds_sum{source="cold",stage="total"} 1.85`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
}

func TestVMStateErrorsShowInTheScrape(t *testing.T) {
	m := New("v")
	m.WatchVMStates([]string{"running"}, func() (map[string]int, error) { return nil, errors.New("database locked") })
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "database locked") {
		t.Errorf("scrape with a failing count = %d %q", rec.Code, rec.Body.String())
	}
}

func TestNilMetricsDoNothing(t *testing.T) {
	var m *Metrics
	m.CreateSucceeded("cold", map[string]float64{"total": 1})
	m.CreateFailed("x")
	m.Destroyed(0, nil)
	m.Exec(0, nil)
	m.FirecrackerCall("x", 0, nil)
	m.APIRequest("GET", "/", 200, 0)
	m.Reaped(1)
	m.Reconciled("kept", 1)
	m.SnapshotTaken("vm", map[string]float64{"total": 1}, 1)
	m.SnapshotFailed("vm", "pause")
	m.SnapshotFlushed(0)
	m.RolledBack(map[string]float64{"total": 1})
	m.Forked(2, 1, map[string]float64{"total": 1})
	m.SnapshotForked(3, 0, map[string]float64{"total": 1})
	m.SnapshotDeleted("garbage")
	m.WatchVMStates(nil, nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil Metrics handler = %d, want 404", rec.Code)
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// formatFloat writes a value the way Prometheus's text format does.
func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
