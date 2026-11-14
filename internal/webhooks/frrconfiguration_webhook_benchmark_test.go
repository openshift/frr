// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"fmt"
	"testing"

	"github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/metallb/frr-k8s/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func BenchmarkValidateConfig(b *testing.B) {
	for _, perNode := range []int{0, 20, 200} {
		for _, broad := range []bool{false, true} {
			b.Run(fmt.Sprintf("unrelated=%d/broad=%t", 99*perNode, broad), func(b *testing.B) { benchmarkValidateConfig(b, perNode, broad) })
		}
	}
}

func benchmarkValidateConfig(b *testing.B, unrelatedPerNode int, broad bool) {
	nodes := generateNodes(100)
	configs := generateFRRConfigurations(nodes[:1], 20)
	unrelated := generateFRRConfigurations(nodes[1:], unrelatedPerNode)
	for i := range unrelated {
		unrelated[i].Name = "unrelated-" + unrelated[i].Name
	}
	configs = append(configs, unrelated...)
	validator := &FRRConfigValidator{index: newNodeConfigIndex()}
	for i := range nodes {
		validator.index.upsertNode(&nodes[i])
	}
	for i := range configs {
		validator.index.upsertConfig(&configs[i])
	}
	originalValidate := Validate
	b.Cleanup(func() { Validate = originalValidate })
	Validate = controller.Validate

	testConfig := &v1beta1.FRRConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: v1beta1.FRRConfigurationSpec{
			NodeSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"node-id": "node-0",
				},
			},
			BGP: v1beta1.BGPConfig{
				Routers: []v1beta1.Router{
					{
						ASN: 65001,
						ID:  "192.168.1.1",
						VRF: "",
						Neighbors: []v1beta1.Neighbor{
							{
								ASN:     65002,
								Address: "192.168.1.2",
								Port:    ptr.To[uint16](179),
							},
						},
						Prefixes: []string{"192.168.1.0/24"},
					},
				},
			},
		},
	}

	if broad {
		testConfig.Spec.NodeSelector = metav1.LabelSelector{}
	}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		_, err := validator.validateConfig(testConfig)
		if err != nil {
			b.Fatalf("validation failed: %v", err)
		}
	}
}

func generateNodes(count int) []corev1.Node {
	nodes := make([]corev1.Node, count)
	for i := range count {
		nodes[i] = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("node-%d", i),
				Labels: map[string]string{
					"node-id": fmt.Sprintf("node-%d", i),
				},
			},
		}
	}
	return nodes
}

func generateFRRConfigurations(nodes []corev1.Node, configsPerNode int) []v1beta1.FRRConfiguration {
	totalConfigs := len(nodes) * configsPerNode
	configs := make([]v1beta1.FRRConfiguration, totalConfigs)

	configIndex := 0
	for _, node := range nodes {
		for range configsPerNode {
			configs[configIndex] = v1beta1.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("frr-config-%d", configIndex),
					Namespace: "default",
				},
				Spec: v1beta1.FRRConfigurationSpec{
					NodeSelector: metav1.LabelSelector{
						MatchLabels: node.Labels,
					},
					BGP: v1beta1.BGPConfig{
						Routers: []v1beta1.Router{
							{
								ASN: 65001,
								ID:  "192.168.1.1",
								VRF: "",
								Neighbors: []v1beta1.Neighbor{
									{
										ASN:     65002,
										Address: "192.168.1.2",
										Port:    ptr.To[uint16](179),
									},
								},
								Prefixes: []string{"192.168.1.0/24"},
							},
						},
					},
				},
			}
			configIndex++
		}
	}
	return configs
}

func BenchmarkNodeConfigIndexEvents(b *testing.B) {
	for _, broad := range []bool{false, true} {
		b.Run(fmt.Sprintf("configUpdate/broad=%t", broad), func(b *testing.B) {
			index := newNodeConfigIndex()
			nodes := generateNodes(100)
			configs := generateFRRConfigurations(nodes, 20)
			for i := range nodes {
				index.upsertNode(&nodes[i])
			}
			for i := range configs {
				index.upsertConfig(&configs[i])
			}
			first := configs[0].DeepCopy()
			second := first.DeepCopy()
			if broad {
				first.Spec.NodeSelector = metav1.LabelSelector{}
			} else {
				first.Spec.NodeSelector.MatchLabels = nodes[0].Labels
			}
			second.Spec.NodeSelector.MatchLabels = nodes[1].Labels
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				index.upsertConfig(first)
				index.upsertConfig(second)
			}
		})
	}
	b.Run("nodeLabelUpdate", func(b *testing.B) {
		index := newNodeConfigIndex()
		nodes := generateNodes(100)
		configs := generateFRRConfigurations(nodes, 20)
		for i := range nodes {
			index.upsertNode(&nodes[i])
		}
		for i := range configs {
			index.upsertConfig(&configs[i])
		}
		changed := nodes[0].DeepCopy()
		changed.Labels = nodes[1].Labels
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			index.upsertNode(changed)
			index.upsertNode(&nodes[0])
		}
	})
}
