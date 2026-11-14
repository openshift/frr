// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/metallb/frr-k8s/api/v1beta1"
	v1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	Logger   log.Logger
	Validate func(resources ...client.ObjectList) error
)

const (
	frrConfigWebhookPath = "/validate-frrk8s-metallb-io-v1beta1-frrconfiguration"
	healthPath           = "/healthz"
)

type FRRConfigValidator struct {
	ClusterResourceNamespace string

	decoder     admission.Decoder
	cacheSynced func() bool
	index       *nodeConfigIndex
}

func (v *FRRConfigValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	v.decoder = admission.NewDecoder(mgr.GetScheme())
	nodes, err := mgr.GetCache().GetInformer(context.Background(), &corev1.Node{}, cache.BlockUntilSynced(false))
	if err != nil {
		return err
	}
	configs, err := mgr.GetCache().GetInformer(context.Background(), &v1beta1.FRRConfiguration{}, cache.BlockUntilSynced(false))
	if err != nil {
		return err
	}
	if err := v.registerIndexHandlers(nodes, configs); err != nil {
		return err
	}

	mgr.GetWebhookServer().Register(
		frrConfigWebhookPath,
		&webhook.Admission{Handler: v})

	mgr.GetWebhookServer().Register(
		healthPath,
		&healthHandler{cacheSynced: v.cacheSynced})

	return nil
}

//+kubebuilder:webhook:verbs=create;update,path=/validate-frrk8s-metallb-io-v1beta1-frrconfiguration,mutating=false,failurePolicy=fail,groups=frrk8s.metallb.io,resources=frrconfigurations,versions=v1beta1,name=frrconfigurationsvalidationwebhook.metallb.io,sideEffects=None,admissionReviewVersions=v1

func (v *FRRConfigValidator) Handle(ctx context.Context, req admission.Request) (resp admission.Response) {
	if !v.cacheSynced() {
		return admission.Errored(http.StatusServiceUnavailable, errors.New("webhook caches have not synced"))
	}
	var config v1beta1.FRRConfiguration
	var oldConfig v1beta1.FRRConfiguration
	if req.Operation == v1.Delete {
		if err := v.decoder.DecodeRaw(req.OldObject, &config); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	} else {
		if err := v.decoder.Decode(req, &config); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if req.OldObject.Size() > 0 {
			if err := v.decoder.DecodeRaw(req.OldObject, &oldConfig); err != nil {
				return admission.Errored(http.StatusBadRequest, err)
			}
		}
	}

	var warnings []string
	switch req.Operation {
	case v1.Create:
		w, err := v.validateConfigCreate(&config)
		if err != nil {
			return admission.Denied(err.Error())
		}
		warnings = w
	case v1.Update:
		w, err := v.validateConfigUpdate(&config)
		if err != nil {
			return admission.Denied(err.Error())
		}
		warnings = w
	case v1.Delete:
		w, err := validateConfigDelete(&config)
		if err != nil {
			return admission.Denied(err.Error())
		}
		warnings = w
	}
	return admission.Allowed("").WithWarnings(warnings...)
}

type healthHandler struct {
	cacheSynced func() bool
}

func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.cacheSynced() {
		http.Error(w, "Webhook caches have not synced", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"status": "ok"}`))
	if err != nil {
		level.Error(Logger).Log("webhook", "healthcheck", "error when writing reply", err)
	}
}

type nodeAndConfigs struct {
	name string
	cfgs *v1beta1.FRRConfigurationList
}

func (v *FRRConfigValidator) validateConfigCreate(frrConfig *v1beta1.FRRConfiguration) ([]string, error) {
	level.Debug(Logger).Log("webhook", "frrconfiguration", "action", "create", "name", frrConfig.Name, "namespace", frrConfig.Namespace)
	defer level.Debug(Logger).Log("webhook", "frrconfiguration", "action", "end create", "name", frrConfig.Name, "namespace", frrConfig.Namespace)

	return v.validateConfig(frrConfig)
}

func (v *FRRConfigValidator) validateConfigUpdate(frrConfig *v1beta1.FRRConfiguration) ([]string, error) {
	level.Debug(Logger).Log("webhook", "frrconfiguration", "action", "update", "name", frrConfig.Name, "namespace", frrConfig.Namespace)
	defer level.Debug(Logger).Log("webhook", "frrconfiguration", "action", "end update", "name", frrConfig.Name, "namespace", frrConfig.Namespace)

	return v.validateConfig(frrConfig)
}

func validateConfigDelete(_ *v1beta1.FRRConfiguration) ([]string, error) {
	return []string{}, nil
}

func (v *FRRConfigValidator) validateConfig(frrConfig *v1beta1.FRRConfiguration) ([]string, error) {
	var warnings []string

	selector, err := metav1.LabelSelectorAsSelector(&frrConfig.Spec.NodeSelector)
	if err != nil {
		return warnings, errors.Join(err, errors.New("resource contains an invalid NodeSelector"))
	}

	if containsDisableMP(frrConfig.Spec.BGP.Routers) {
		warnings = append(warnings, "disableMP is deprecated, is ignored and will be removed in a future release")
	}

	matchingNodes, groupingWarnings := v.index.configsPerMatchingNode(frrConfig, selector)
	warnings = append(warnings, groupingWarnings...)

	for _, n := range matchingNodes {
		err := Validate(n.cfgs)
		if err != nil {
			return warnings, errors.Join(err, fmt.Errorf("resource is invalid for node %s", n.name))
		}
	}

	return warnings, nil
}

// gracefulRestartChangeWarnings reports a graceful restart change that only
// takes effect once FRR is restarted.
func gracefulRestartChangeWarnings(oldCfg, newCfg v1beta1.FRRConfiguration) []string {
	var warnings []string
	for _, rold := range oldCfg.Spec.BGP.Routers {
		for _, rnew := range newCfg.Spec.BGP.Routers {
			if rold.VRF != rnew.VRF {
				continue
			}
			for _, nold := range rold.Neighbors {
				for _, nnew := range rnew.Neighbors {
					if nold.ASN == nnew.ASN && nold.Address == nnew.Address && nold.EnableGracefulRestart != nnew.EnableGracefulRestart {
						warnings = append(warnings, "Graceful restart configuration changed, it will be available on the next restart")
					}
				}
			}
		}
	}
	return warnings
}

func containsDisableMP(routers []v1beta1.Router) bool {
	for _, r := range routers {
		for _, n := range r.Neighbors {
			//nolint:staticcheck // DisableMP is deprecated but still supported for backward compatibility
			if n.DisableMP {
				return true
			}
		}
	}
	return false
}
