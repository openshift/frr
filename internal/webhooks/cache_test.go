// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/metallb/frr-k8s/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestWebhookWaitsForCacheSync(t *testing.T) {
	synced := false
	ready := func() bool { return synced }
	validator := &FRRConfigValidator{cacheSynced: ready}
	response := validator.Handle(t.Context(), admission.Request{})
	if response.Allowed || response.Result.Code != http.StatusServiceUnavailable {
		t.Fatalf("unsynced caches must reject admission: %#v", response)
	}

	health := &healthHandler{cacheSynced: ready}
	for _, expected := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		result := httptest.NewRecorder()
		health.ServeHTTP(result, httptest.NewRequest(http.MethodGet, healthPath, nil))
		if result.Code != expected {
			t.Fatalf("synced=%t: health status=%d, want %d", synced, result.Code, expected)
		}
		synced = true
	}
}

func TestValidateCachedConfigurations(t *testing.T) {
	source := v1beta1.FRRConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: TestNamespace},
		Spec: v1beta1.FRRConfigurationSpec{BGP: v1beta1.BGPConfig{Routers: []v1beta1.Router{{
			ASN: 65001, Neighbors: []v1beta1.Neighbor{{
				ASN: 65002, Address: "192.0.2.1", PasswordSecret: v1beta1.SecretReference{Name: "peer-secret", Namespace: TestNamespace},
			}},
		}}}},
	}
	validator := &FRRConfigValidator{index: newNodeConfigIndex()}
	validator.index.upsertNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}})
	validator.index.upsertConfig(&source)
	originalValidate := Validate
	t.Cleanup(func() { Validate = originalValidate })
	Validate = controller.Validate

	incoming := source.DeepCopy()
	incoming.Name = "incoming"
	before := source.DeepCopy()
	incomingBefore := incoming.DeepCopy()
	if _, err := validator.validateConfig(incoming); err != nil {
		t.Fatalf("valid configuration was rejected: %v", err)
	}
	if diff := cmp.Diff(before, &source); diff != "" {
		t.Fatalf("validation mutated cached configuration (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(incomingBefore, incoming); diff != "" {
		t.Fatalf("validation mutated incoming configuration (-before +after):\n%s", diff)
	}

	conflicting := source.DeepCopy()
	conflicting.Spec.BGP.Routers[0].ASN++
	validator.index.upsertConfig(conflicting)
	conflictingBefore := conflicting.DeepCopy()
	if _, err := validator.validateConfig(incoming); err == nil {
		t.Fatal("conflicting cached configuration was ignored")
	}
	if diff := cmp.Diff(conflictingBefore, conflicting); diff != "" {
		t.Fatalf("failed validation mutated cached configuration (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(incomingBefore, incoming); diff != "" {
		t.Fatalf("failed validation mutated incoming configuration (-before +after):\n%s", diff)
	}
}
