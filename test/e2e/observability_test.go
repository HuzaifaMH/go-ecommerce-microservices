//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests check that the system can be observed: a request leaves one
// connected trace across every service it touched, the metrics are scraped and
// the alert rules are loaded. They need the monitoring part of the compose
// stack (Jaeger, Prometheus, Grafana), which `make up` starts.

func jaegerURL() string     { return env("E2E_JAEGER_URL", "http://localhost:16686") }
func prometheusURL() string { return env("E2E_PROMETHEUS_URL", "http://localhost:9099") }
func grafanaURL() string    { return env("E2E_GRAFANA_URL", "http://localhost:3000") }

// getJSON fetches url and decodes the JSON body into out. It returns the status code.
func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v (is the monitoring stack running? try `make up`)", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("GET %s: decoding %q: %v", url, string(body), err)
		}
	}
	return resp.StatusCode
}

type jaegerTrace struct {
	TraceID string `json:"traceID"`
	Spans   []struct {
		OperationName string `json:"operationName"`
		ProcessID     string `json:"processID"`
	} `json:"spans"`
	Processes map[string]struct {
		ServiceName string `json:"serviceName"`
	} `json:"processes"`
}

func (j jaegerTrace) services() []string {
	set := map[string]bool{}
	for _, s := range j.Spans {
		set[j.Processes[s.ProcessID].ServiceName] = true
	}
	var out []string
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (j jaegerTrace) hasOperation(name string) bool {
	for _, s := range j.Spans {
		if s.OperationName == name {
			return true
		}
	}
	return false
}

// waitTrace polls Jaeger until the trace contains every wanted operation, then returns it.
func waitTrace(t *testing.T, traceID string, wantOps ...string) jaegerTrace {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	var last jaegerTrace
	for time.Now().Before(deadline) {
		var resp struct {
			Data []jaegerTrace `json:"data"`
		}
		if getJSON(t, jaegerURL()+"/api/traces/"+traceID, &resp) == http.StatusOK && len(resp.Data) == 1 {
			last = resp.Data[0]
			complete := true
			for _, op := range wantOps {
				complete = complete && last.hasOperation(op)
			}
			if complete {
				return last
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	var have []string
	for _, s := range last.Spans {
		have = append(have, s.OperationName)
	}
	t.Fatalf("trace %s never contained %v; it has %d spans: %v", traceID, wantOps, len(last.Spans), have)
	return jaegerTrace{}
}

// orderWithTrace places an order through the gateway and returns its ID and the trace ID
// the gateway reported for the request.
func orderWithTrace(g *gateway, customer, sku string) (orderID, traceID string) {
	g.t.Helper()
	tok := g.token(customer)
	resp := g.call("POST", "/v1/orders", tok, map[string]string{"Idempotency-Key": unique("key")},
		map[string]any{"items": []map[string]any{{"sku": sku, "quantity": 1}}})
	if resp.status != http.StatusAccepted {
		g.t.Fatalf("create order: %d %s", resp.status, resp.rawBody)
	}
	traceID = resp.header.Get("X-Trace-Id")
	if len(traceID) != 32 {
		g.t.Fatalf("X-Trace-Id = %q, want a 32-hex-digit trace ID on every response", traceID)
	}
	orderID, _ = resp.body["id"].(string)
	return orderID, traceID
}

func TestOneOrderIsOneTraceAcrossEveryService(t *testing.T) {
	g := connectGateway(t)
	id, traceID := orderWithTrace(g, unique("alice"), "MUG-GOPHER-001")

	// The request, the saga's hops through the broker, and the notification all
	// belong to the trace started by the HTTP request.
	trace := waitTrace(t, traceID,
		"POST /v1/orders",
		"ecommerce.order.v1.OrderService/CreateOrder",
		"ecommerce.inventory.v1.InventoryService/GetStock", // order pricing the items
		"publish inventory.cmd.reserve", "consume inventory.cmd.reserve",
		"publish inventory.evt.reserved", "consume inventory.evt.reserved",
		"publish payment.cmd.charge", "consume payment.cmd.charge",
		"publish payment.evt.succeeded", "consume payment.evt.succeeded",
		"publish order.evt.confirmed", "consume order.evt.confirmed",
	)

	want := []string{"api-gateway", "inventory-service", "notification-service", "order-service", "payment-service"}
	if got := trace.services(); !slices.Equal(got, want) {
		t.Fatalf("the trace spans services %v, want all of %v", got, want)
	}

	// The trace can be found by the business ID, not just the trace ID.
	deadline := time.Now().Add(20 * time.Second)
	query := url.Values{"service": {"api-gateway"}, "limit": {"5"}, "tags": {fmt.Sprintf(`{"order.id":%q}`, id)}}
	for {
		var found struct {
			Data []jaegerTrace `json:"data"`
		}
		getJSON(t, jaegerURL()+"/api/traces?"+query.Encode(), &found)
		if len(found.Data) == 1 && found.Data[0].TraceID == traceID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("searching Jaeger for order.id=%s found %d traces, want the order's trace %s", id, len(found.Data), traceID)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestACompensatedOrderShowsTheRollbackInItsTrace(t *testing.T) {
	g := connectGateway(t)
	_, traceID := orderWithTrace(g, unique("decline-bob"), "TSHIRT-GO-M") // the payment will be declined

	// The failure and its compensation are in the trace: payment failed, stock released, order cancelled, customer told.
	trace := waitTrace(t, traceID,
		"publish payment.evt.failed", "consume payment.evt.failed",
		"publish inventory.cmd.release", "consume inventory.cmd.release",
		"publish order.evt.cancelled", "consume order.evt.cancelled",
	)
	if trace.hasOperation("publish order.evt.confirmed") {
		t.Error("a cancelled order must not also be confirmed")
	}
	if got := trace.services(); !slices.Contains(got, "notification-service") {
		t.Errorf("services = %v: the customer notification must be part of the trace", got)
	}
}

func TestEveryResponseCarriesATraceID(t *testing.T) {
	g := connectGateway(t)
	for name, resp := range map[string]response{
		"public catalogue": g.call("GET", "/v1/products", "", nil, nil),
		"an error":         g.call("GET", "/v1/orders", "", nil, nil),
		"unknown path":     g.call("GET", "/nope", "", nil, nil),
	} {
		if len(resp.header.Get("X-Trace-Id")) != 32 || resp.header.Get("X-Request-Id") == "" {
			t.Errorf("%s: X-Trace-Id=%q X-Request-Id=%q; support needs both", name, resp.header.Get("X-Trace-Id"), resp.header.Get("X-Request-Id"))
		}
	}
}

// --- metrics ---

type promVector struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// promValue runs an instant query and returns the first sample, or ok == false when there is none.
func promValue(t *testing.T, query string) (value float64, ok bool) {
	t.Helper()
	var v promVector
	if getJSON(t, prometheusURL()+"/api/v1/query?query="+url.QueryEscape(query), &v) != http.StatusOK || v.Status != "success" {
		t.Fatalf("query %q failed", query)
	}
	if len(v.Data.Result) == 0 {
		return 0, false
	}
	s, _ := v.Data.Result[0].Value[1].(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("query %q returned %q", query, s)
	}
	return f, true
}

func TestPrometheusScrapesEveryService(t *testing.T) {
	connectGateway(t)

	want := []string{"api-gateway", "inventory-service", "notification-service", "order-service", "payment-service"}
	deadline := time.Now().Add(45 * time.Second)
	for {
		var resp struct {
			Data struct {
				ActiveTargets []struct {
					Labels map[string]string `json:"labels"`
					Health string            `json:"health"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		getJSON(t, prometheusURL()+"/api/v1/targets", &resp)
		up := map[string]bool{}
		for _, tg := range resp.Data.ActiveTargets {
			if tg.Health == "up" {
				up[tg.Labels["service"]] = true
			}
		}
		var missing []string
		for _, s := range want {
			if !up[s] {
				missing = append(missing, s)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("targets not up: %v", missing)
		}
		time.Sleep(time.Second)
	}
}

func TestBusinessMetricsFollowRealOrders(t *testing.T) {
	g := connectGateway(t)
	before, _ := promValue(t, `sum(orders_finished_total{outcome="confirmed"})`)

	id, _ := orderWithTrace(g, unique("alice"), "BOOK-GRPC-001")
	g.waitOrder(g.token("root", "admin"), id, "confirmed")

	// Prometheus scrapes every 5 seconds: wait for the counter to move.
	deadline := time.Now().Add(40 * time.Second)
	for {
		if now, ok := promValue(t, `sum(orders_finished_total{outcome="confirmed"})`); ok && now > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orders_finished_total{outcome=\"confirmed\"} never increased after a confirmed order")
		}
		time.Sleep(time.Second)
	}

	// The other services' business metrics and the plumbing metrics exist too.
	for _, q := range []string{
		`sum(orders_created_total)`,
		`sum(payments_total{outcome="succeeded"})`,
		`sum(stock_reservations_total{outcome="reserved"})`,
		`sum(notifications_total{outcome="sent"})`,
		`sum(messaging_consumed_total{outcome="ack"})`,
		`sum(messaging_published_total{result="ok"})`,
		`sum(grpc_server_handled_total{grpc_code="OK"})`,
		`sum(http_requests_total{status="202"})`,
		`count(outbox_pending_messages)`,
		`count(order_saga_duration_seconds_bucket)`,
	} {
		if v, ok := promValue(t, q); !ok || v <= 0 {
			t.Errorf("%s = %v (present: %v), want a positive value", q, v, ok)
		}
	}
}

func TestAlertRulesAreLoaded(t *testing.T) {
	connectGateway(t)
	var resp struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	getJSON(t, prometheusURL()+"/api/v1/rules", &resp)

	have := map[string]bool{}
	for _, g := range resp.Data.Groups {
		for _, r := range g.Rules {
			if r.Type == "alerting" {
				have[r.Name] = true
			}
		}
	}
	for _, name := range []string{"ServiceDown", "OutboxStuck", "OrderSagaTimeouts", "RefundRequired",
		"MessagesTerminated", "OrderCancellationRateHigh", "PaymentProviderErrors", "GatewayHighErrorRate"} {
		if !have[name] {
			t.Errorf("alert rule %s is not loaded", name)
		}
	}
}

func TestMetricsAreNotExposedOnThePublicPort(t *testing.T) {
	connectGateway(t)

	if code := getJSON(t, gatewayURL()+"/metrics", nil); code != http.StatusNotFound {
		t.Fatalf("GET %s/metrics = %d: the public gateway port must not serve metrics", gatewayURL(), code)
	}

	// The internal metrics port does, and only exposes this system's own metrics.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, env("E2E_GATEWAY_METRICS_URL", "http://localhost:9100")+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("the gateway's internal metrics port is not published on this host: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "http_requests_total") {
		t.Fatalf("internal metrics port: %d", resp.StatusCode)
	}
}

func TestGrafanaServesTheProvisionedDashboard(t *testing.T) {
	connectGateway(t)

	var health struct {
		Database string `json:"database"`
	}
	if code := getJSON(t, grafanaURL()+"/api/health", &health); code != http.StatusOK || health.Database != "ok" {
		t.Fatalf("grafana health: %d %+v", code, health)
	}
	var dash struct {
		Dashboard struct {
			Title  string `json:"title"`
			Panels []struct {
				Title string `json:"title"`
			} `json:"panels"`
		} `json:"dashboard"`
		Meta struct {
			Provisioned bool `json:"provisioned"`
		} `json:"meta"`
	}
	if code := getJSON(t, grafanaURL()+"/api/dashboards/uid/ecommerce-overview", &dash); code != http.StatusOK {
		t.Fatalf("dashboard: HTTP %d", code)
	}
	if dash.Dashboard.Title != "E-Commerce overview" || !dash.Meta.Provisioned || len(dash.Dashboard.Panels) < 15 {
		t.Fatalf("dashboard = %+v", dash)
	}
}
