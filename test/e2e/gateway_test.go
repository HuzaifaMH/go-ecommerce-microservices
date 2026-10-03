//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests drive the system the way a real client does: over the public
// REST API of the gateway, with a bearer token. They need DEV_AUTH on (the
// compose stack has it) so a token can be minted at POST /dev/token.

func gatewayURL() string { return env("E2E_GATEWAY_URL", "http://localhost:8080") }

type gateway struct{ t *testing.T }

func connectGateway(t *testing.T) *gateway {
	t.Helper()
	waitReady(t, gatewayURL()+"/readyz")
	waitReady(t, env("E2E_ORDER_HEALTH", "http://localhost:8083/readyz"))
	waitReady(t, env("E2E_NOTIFICATION_HEALTH", "http://localhost:8084/readyz"))
	return &gateway{t: t}
}

type response struct {
	status  int
	header  http.Header
	body    map[string]any
	rawBody string
}

// call sends one request. token and extra headers are optional; body may be a Go value or "".
func (g *gateway) call(method, path, token string, headers map[string]string, body any) response {
	g.t.Helper()
	var reader io.Reader
	if body != nil {
		var b []byte
		switch v := body.(type) {
		case string:
			b = []byte(v)
		default:
			var err error
			if b, err = json.Marshal(v); err != nil {
				g.t.Fatal(err)
			}
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, gatewayURL()+path, reader)
	if err != nil {
		g.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	_ = json.Unmarshal(raw, &out.body) // not every response is a JSON object
	return out
}

// token mints a development token for subject.
func (g *gateway) token(subject string, roles ...string) string {
	g.t.Helper()
	resp := g.call("POST", "/dev/token", "", nil, map[string]any{"subject": subject, "roles": roles})
	if resp.status != http.StatusOK {
		g.t.Fatalf("could not get a dev token (is DEV_AUTH on?): %d %s", resp.status, resp.rawBody)
	}
	tok, _ := resp.body["access_token"].(string)
	if tok == "" {
		g.t.Fatalf("no access_token in %s", resp.rawBody)
	}
	return tok
}

// placeOrder creates an order for the token's owner and returns its ID.
func (g *gateway) placeOrder(token, key, sku string, qty int) string {
	g.t.Helper()
	resp := g.call("POST", "/v1/orders", token, map[string]string{"Idempotency-Key": key},
		map[string]any{"items": []map[string]any{{"sku": sku, "quantity": qty}}})
	if resp.status != http.StatusAccepted {
		g.t.Fatalf("create order: %d %s", resp.status, resp.rawBody)
	}
	id, _ := resp.body["id"].(string)
	if id == "" || resp.header.Get("Location") != "/v1/orders/"+id {
		g.t.Fatalf("id %q, Location %q", id, resp.header.Get("Location"))
	}
	return id
}

// waitOrder polls the order until it has the wanted status.
func (g *gateway) waitOrder(token, id, want string) map[string]any {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last response
	for time.Now().Before(deadline) {
		last = g.call("GET", "/v1/orders/"+id, token, nil, nil)
		if last.status == http.StatusOK && last.body["status"] == want {
			return last.body
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatalf("order %s never reached %q; last: %d %s", id, want, last.status, last.rawBody)
	return nil
}

// waitNotificationCount polls the order's notifications until there are want, none pending.
func (g *gateway) waitNotifications(token, orderID string, want int) []map[string]any {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last response
	for time.Now().Before(deadline) {
		last = g.call("GET", "/v1/orders/"+orderID+"/notifications", token, nil, nil)
		items, _ := last.body["notifications"].([]any)
		pending := 0
		var out []map[string]any
		for _, it := range items {
			n, _ := it.(map[string]any)
			out = append(out, n)
			if n["status"] == "pending" {
				pending++
			}
		}
		if last.status == http.StatusOK && len(out) == want && pending == 0 {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatalf("order %s: want %d final notifications; last: %d %s", orderID, want, last.status, last.rawBody)
	return nil
}

func TestGatewayCustomerJourney(t *testing.T) {
	g := connectGateway(t)

	// Browse the catalogue without logging in.
	products := g.call("GET", "/v1/products?page_size=50", "", nil, nil)
	if products.status != http.StatusOK || !strings.Contains(products.rawBody, "MUG-GOPHER-001") {
		t.Fatalf("catalogue: %d %s", products.status, products.rawBody)
	}
	one := g.call("GET", "/v1/products/MUG-GOPHER-001", "", nil, nil)
	if one.status != http.StatusOK || one.body["sku"] != "MUG-GOPHER-001" {
		t.Fatalf("product: %d %s", one.status, one.rawBody)
	}

	// Log in (development token) and order.
	customer := unique("alice")
	tok := g.token(customer)
	id := g.placeOrder(tok, unique("key"), "MUG-GOPHER-001", 2)

	confirmed := g.waitOrder(tok, id, "confirmed")
	if confirmed["customer_id"] != customer {
		t.Errorf("customer_id = %v, want the token's subject %q", confirmed["customer_id"], customer)
	}
	total, _ := confirmed["total"].(map[string]any)
	if total["amount_minor"] != float64(2*1299) {
		t.Errorf("total = %v, want 2598 priced by the catalogue", total)
	}

	// The customer was told, and can see it.
	notes := g.waitNotifications(tok, id, 1)
	if notes[0]["channel"] != "email" || notes[0]["status"] != "sent" || notes[0]["kind"] != "order_confirmed" {
		t.Errorf("notification = %v", notes[0])
	}

	// And the order is in their list.
	list := g.call("GET", "/v1/orders", tok, nil, nil)
	if list.status != http.StatusOK || !strings.Contains(list.rawBody, id) {
		t.Errorf("list: %d %s", list.status, list.rawBody)
	}
}

func TestGatewayRequiresAuthentication(t *testing.T) {
	g := connectGateway(t)

	for name, tok := range map[string]string{"no token": "", "garbage token": "not.a.token"} {
		resp := g.call("GET", "/v1/orders", tok, nil, nil)
		if resp.status != http.StatusUnauthorized || resp.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d %s", name, resp.status, resp.rawBody)
		}
	}
	resp := g.call("POST", "/v1/orders", "", map[string]string{"Idempotency-Key": "k"}, map[string]any{"items": []any{}})
	if resp.status != http.StatusUnauthorized {
		t.Errorf("creating an order without a token: %d", resp.status)
	}
}

func TestGatewayOrdersBelongToTheirOwner(t *testing.T) {
	g := connectGateway(t)
	alice, bob := g.token(unique("alice")), g.token(unique("bob"))
	admin := g.token(unique("root"), "admin")
	id := g.placeOrder(alice, unique("key"), "BOOK-GO-001", 1)
	g.waitOrder(alice, id, "confirmed")

	// Bob cannot see, cancel, or read the notifications of Alice's order, and the
	// answers look exactly like those for an order that does not exist.
	missing := g.call("GET", "/v1/orders/does-not-exist", bob, nil, nil)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/orders/" + id},
		{"POST", "/v1/orders/" + id + "/cancel"},
		{"GET", "/v1/orders/" + id + "/notifications"},
	} {
		resp := g.call(c.method, c.path, bob, nil, nil)
		if resp.status != http.StatusNotFound {
			t.Errorf("bob %s %s: %d, want 404", c.method, c.path, resp.status)
		}
	}
	foreign := g.call("GET", "/v1/orders/"+id, bob, nil, nil)
	fe, _ := foreign.body["error"].(map[string]any)
	me, _ := missing.body["error"].(map[string]any)
	if fe["message"] != me["message"] {
		t.Errorf("a foreign order and a missing one must be indistinguishable: %q vs %q", fe["message"], me["message"])
	}

	// Bob's list does not contain it, and he may not ask for Alice's.
	if list := g.call("GET", "/v1/orders", bob, nil, nil); strings.Contains(list.rawBody, id) {
		t.Error("bob's list contains alice's order")
	}
	if resp := g.call("GET", "/v1/orders?customer_id="+alice, bob, nil, nil); resp.status != http.StatusForbidden {
		t.Errorf("listing someone else's orders: %d, want 403", resp.status)
	}

	// An admin can see it.
	if resp := g.call("GET", "/v1/orders/"+id, admin, nil, nil); resp.status != http.StatusOK {
		t.Errorf("admin: %d", resp.status)
	}
	// Alice's order is untouched by all of that.
	g.waitOrder(alice, id, "confirmed")
}

func TestGatewayNeverTrustsTheBodyForWhoOrdersOrWhatItCosts(t *testing.T) {
	g := connectGateway(t)
	tok := g.token(unique("mallory"))

	for name, body := range map[string]string{
		"customer_id": `{"customer_id":"victim","items":[{"sku":"BOOK-GO-001","quantity":1}]}`,
		"unit_price":  `{"items":[{"sku":"BOOK-GO-001","quantity":1,"unit_price":{"currency_code":"USD","amount_minor":1}}]}`,
	} {
		resp := g.call("POST", "/v1/orders", tok, map[string]string{"Idempotency-Key": unique("key")}, body)
		if resp.status != http.StatusBadRequest {
			t.Errorf("%s in the body: %d, want 400", name, resp.status)
		}
	}
}

func TestGatewayIdempotency(t *testing.T) {
	g := connectGateway(t)
	tok := g.token(unique("alice"))
	key := unique("key")

	first := g.placeOrder(tok, key, "BOOK-GRPC-001", 1)
	again := g.placeOrder(tok, key, "BOOK-GRPC-001", 1)
	if first != again {
		t.Fatalf("a repeated request created order %s, want the original %s", again, first)
	}

	resp := g.call("POST", "/v1/orders", tok, map[string]string{"Idempotency-Key": key},
		map[string]any{"items": []map[string]any{{"sku": "BOOK-GRPC-001", "quantity": 5}}})
	if resp.status != http.StatusConflict {
		t.Errorf("reusing the key for a different request: %d, want 409", resp.status)
	}

	missing := g.call("POST", "/v1/orders", tok, nil, map[string]any{"items": []map[string]any{{"sku": "BOOK-GRPC-001", "quantity": 1}}})
	if missing.status != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key: %d, want 400", missing.status)
	}
}

func TestGatewayDeclinedPaymentIsExplainedToTheCustomer(t *testing.T) {
	g := connectGateway(t)
	tok := g.token(unique("decline-bob"))

	id := g.placeOrder(tok, unique("key"), "TSHIRT-GO-M", 1)
	order := g.waitOrder(tok, id, "cancelled")
	if order["cancel_reason"] != "payment failed: card declined" {
		t.Errorf("cancel_reason = %v", order["cancel_reason"])
	}

	notes := g.waitNotifications(tok, id, 2)
	channels := map[any]bool{}
	for _, n := range notes {
		channels[n["channel"]] = true
		if n["status"] != "sent" || !strings.Contains(fmt.Sprint(n["body"]), "payment") {
			t.Errorf("notification = %v", n)
		}
	}
	if !channels["email"] || !channels["sms"] {
		t.Errorf("channels = %v, want email and sms", channels)
	}
}

func TestGatewayCancelsAnOrderBeforeConfirmationOnly(t *testing.T) {
	g := connectGateway(t)
	tok := g.token(unique("alice"))
	id := g.placeOrder(tok, unique("key"), "BOOK-GO-001", 1)
	g.waitOrder(tok, id, "confirmed")

	resp := g.call("POST", "/v1/orders/"+id+"/cancel", tok, nil, map[string]any{"reason": "too late"})
	if resp.status != http.StatusConflict {
		t.Fatalf("cancelling a confirmed order: %d %s, want 409", resp.status, resp.rawBody)
	}
}

func TestGatewayErrorsAreConsistentAndTraceable(t *testing.T) {
	g := connectGateway(t)
	tok := g.token(unique("alice"))

	// A client-chosen request ID is echoed and appears in the error.
	resp := g.call("POST", "/v1/orders", tok, map[string]string{"Idempotency-Key": unique("key"), "X-Request-Id": "e2e-trace-1"},
		map[string]any{"items": []map[string]any{{"sku": "NOT-A-SKU", "quantity": 1}}})
	if resp.status != http.StatusBadRequest || resp.header.Get("X-Request-Id") != "e2e-trace-1" {
		t.Fatalf("unknown SKU: %d, request id %q", resp.status, resp.header.Get("X-Request-Id"))
	}
	e, _ := resp.body["error"].(map[string]any)
	if e["code"] != "invalid_argument" || e["request_id"] != "e2e-trace-1" || !strings.Contains(fmt.Sprint(e["message"]), "NOT-A-SKU") {
		t.Errorf("error = %v", e)
	}

	if nf := g.call("GET", "/v1/no-such-endpoint", "", nil, nil); nf.status != http.StatusNotFound || nf.body["error"] == nil {
		t.Errorf("unknown endpoint: %d %s", nf.status, nf.rawBody)
	}
	if nf := g.call("GET", "/v1/products/NOPE", "", nil, nil); nf.status != http.StatusNotFound {
		t.Errorf("unknown product: %d", nf.status)
	}
}

func TestGatewayReportsReadinessOfItsBackends(t *testing.T) {
	g := connectGateway(t)
	resp := g.call("GET", "/readyz", "", nil, nil)
	checks, _ := resp.body["checks"].(map[string]any)
	if resp.status != http.StatusOK || checks["order-service"] != "ok" || checks["inventory-service"] != "ok" || checks["notification-service"] != "ok" {
		t.Fatalf("readyz: %d %s", resp.status, resp.rawBody)
	}
}
