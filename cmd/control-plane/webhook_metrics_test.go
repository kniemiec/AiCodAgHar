package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kniemiec/AiCodAgHar/internal/clients/gitclient"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/domain"
	"github.com/kniemiec/AiCodAgHar/internal/control-plane/instrumented"
)

// MT-14: each delivery is counted once, by outcome.
func TestHandleForgejoWebhook_CountsOutcomes(t *testing.T) {
	s, git, store := newWebhookTestServer(t)
	reader := sdkmetric.NewManualReader()
	s.metrics = instrumented.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), instrumented.Options{})
	mux := newServeMux(s)
	repo := gitclient.RepoRef{Owner: "acme", Name: "widgets"}
	rec, _, _ := store.GetTask(context.Background(), "task-1")
	issue := *rec.State.ApprovalIssueNumber

	// rejected: bad signature
	req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(labelWebhookBody(t, issue, "alice")))
	req.Header.Set("X-Forgejo-Signature", "not-a-real-signature")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	// ignored: no approval label yet
	postSignedWebhook(t, mux, labelWebhookBody(t, issue, "alice"))
	// rejected: labeler isn't a listed approver
	git.SetIssueLabels(repo, issue, []string{"approved"})
	postSignedWebhook(t, mux, labelWebhookBody(t, issue, "mallory"))
	// processed: a real approval
	postSignedWebhook(t, mux, labelWebhookBody(t, issue, "alice"))
	waitForStatus(t, store, "task-1", domain.TaskStatusStopSuccess)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "harness_webhook_events_total" {
				for _, dp := range s.DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key("outcome"))
					got[v.AsString()] += dp.Value
				}
			}
		}
	}
	want := map[string]int64{instrumented.WebhookRejected: 2, instrumented.WebhookIgnored: 1, instrumented.WebhookProcessed: 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("harness_webhook_events_total{outcome=%s} = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if got[instrumented.WebhookFailed] != 0 {
		t.Errorf("unexpected failed deliveries: %v", got)
	}
}
