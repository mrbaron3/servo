package control

import (
	"context"
	"strings"
	"testing"
)

func TestRuntimeTopologyFailsClosedWhenOmitted(t *testing.T) {
	var topology RuntimeTopology
	for _, component := range []string{
		ComponentIssueMonitor,
		ComponentPRMonitor,
		ComponentForwarder,
		ComponentExecution,
		ComponentQueue,
	} {
		if topology.ManagesComponent(component) || (&Store{}).ManagesComponent(component) {
			t.Fatalf("omitted topology managed component %q", component)
		}
	}
}

func TestRuntimeTopologiesMakeLegacyForwarderOwnershipExplicit(t *testing.T) {
	if !RuntimeTopologyLegacyCLIForwarder.ManagesComponent(ComponentForwarder) {
		t.Fatal("explicit legacy topology did not manage its compatibility forwarder")
	}
	if RuntimeTopologySignedWebhookIngress.ManagesComponent(ComponentForwarder) {
		t.Fatal("signed-ingress topology managed the legacy forwarder")
	}
	if !RuntimeTopologySignedWebhookIngress.ManagesComponent(ComponentIssueMonitor) {
		t.Fatal("signed-ingress topology omitted the Issue Monitor")
	}
	for _, topology := range []RuntimeTopology{
		RuntimeTopologyLegacyCLIForwarder,
		RuntimeTopologySignedWebhookIngress,
	} {
		if topology.ManagesComponent("future_component") {
			t.Fatalf("topology %q silently managed an unknown component", topology)
		}
	}
}

func TestOpenStoreRejectsImplicitTopologyBeforeConnecting(t *testing.T) {
	store, err := OpenStore(
		context.Background(),
		"postgresql://127.0.0.1:1/unreachable",
		"/unused",
	)
	if store != nil || err == nil || !strings.Contains(err.Error(), "runtime topology is required") {
		t.Fatalf("OpenStore() = %#v, %v", store, err)
	}
}
