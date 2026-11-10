// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/metallb/frr-k8s/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// Return aliased objects, as informer reads with UnsafeDisableDeepCopy do.
// Validation must copy these objects before it resets secret references.
type webhookCacheReader struct {
	client.Reader
	nodes   *corev1.NodeList
	configs *v1beta1.FRRConfigurationList
}

func (r *webhookCacheReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch out := list.(type) {
	case *corev1.NodeList:
		*out = *r.nodes
	case *v1beta1.FRRConfigurationList:
		*out = *r.configs
	default:
		return fmt.Errorf("unexpected list type %T", list)
	}
	return nil
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
	reader := &webhookCacheReader{
		nodes:   &corev1.NodeList{Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}}},
		configs: &v1beta1.FRRConfigurationList{Items: []v1beta1.FRRConfiguration{source}},
	}
	originalClient, originalValidate := WebhookClient, Validate
	t.Cleanup(func() { WebhookClient, Validate = originalClient, originalValidate })
	WebhookClient, Validate = reader, controller.Validate

	incoming := source.DeepCopy()
	incoming.Name = "incoming"
	incoming.Spec.BGP.Routers[0].Neighbors[0].PasswordSecret = v1beta1.SecretReference{}
	incoming.Spec.BGP.Routers[0].Neighbors[0].Password = "password"
	before := reader.configs.DeepCopy()
	if _, err := validateConfig(incoming); err != nil {
		t.Fatalf("valid configuration was rejected: %v", err)
	}
	if diff := cmp.Diff(before, reader.configs); diff != "" {
		t.Fatalf("validation mutated cached configurations (-before +after):\n%s", diff)
	}

	reader.configs = before.DeepCopy()
	reader.configs.Items[0].Spec.BGP.Routers[0].ASN++
	if _, err := validateConfig(incoming); err == nil {
		t.Fatal("conflicting cached configuration was ignored")
	}
}
