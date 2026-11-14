// SPDX-License-Identifier:Apache-2.0

package controller

import (
	"net"

	v1beta1 "github.com/metallb/frr-k8s/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TransientError is an error that happens due to interdependencies
// between crds, such as referencing non-existing secrets.
// Since we don't want webhooks to make assumptions on ordering, we reset the
// fields that could cause a transient error from configurations before validating them.
type TransientError struct {
	Message string
}

func (e TransientError) Error() string { return e.Message }

func Validate(resources ...client.ObjectList) error {
	clusterResources := ClusterResources{
		FRRConfigs: make([]v1beta1.FRRConfiguration, 0),
	}

	for _, list := range resources {
		if l, ok := list.(*v1beta1.FRRConfigurationList); ok {
			// Validation resets secrets and may merge nested routing state.
			// Copy only the selected configurations, leaving informer and caller objects immutable.
			for i := range l.Items {
				clusterResources.FRRConfigs = append(clusterResources.FRRConfigs, *l.Items[i].DeepCopy())
			}
		}
	}
	resetSecrets(clusterResources.FRRConfigs)

	_, err := apiToFRR(clusterResources, []net.IPNet{})
	return err
}

// Resets the secrets fields of the given configurations as they can cause a transient error.
func resetSecrets(cfgs []v1beta1.FRRConfiguration) {
	for _, cfg := range cfgs {
		for _, r := range cfg.Spec.BGP.Routers {
			for i := range r.Neighbors {
				r.Neighbors[i].PasswordSecret = v1beta1.SecretReference{}
			}
		}
	}
}
