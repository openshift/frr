// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/metallb/frr-k8s/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func indexConfig(namespace, name string, selector metav1.LabelSelector) *v1beta1.FRRConfiguration {
	return &v1beta1.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: v1beta1.FRRConfigurationSpec{NodeSelector: selector}}
}

func assertIndexGroups(t *testing.T, idx *nodeConfigIndex, incoming *v1beta1.FRRConfiguration, expected map[string][]string) {
	t.Helper()
	selector, err := metav1.LabelSelectorAsSelector(&incoming.Spec.NodeSelector)
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := idx.configsPerMatchingNode(incoming, selector)
	got := map[string][]string{}
	names := []string{}
	for _, group := range groups {
		names = append(names, group.name)
		for _, cfg := range group.cfgs.Items {
			got[group.name] = append(got[group.name], cfg.Namespace+"/"+cfg.Name)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("nodes are not sorted: %v", names)
	}
	if diff := cmp.Diff(expected, got); diff != "" {
		t.Fatalf("unexpected memberships (-want +got):\n%s", diff)
	}
	// Check reverse memberships as well: deletion depends on these staying exact.
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for key, cfg := range idx.configs {
		for name, node := range idx.nodes {
			_, reverse := cfg.nodes[name]
			_, forward := slices.BinarySearch(node.configKeys, key)
			if reverse != forward {
				t.Fatalf("inconsistent membership %s -> %s", name, key)
			}
		}
	}
}

func TestNodeConfigIndexEvents(t *testing.T) {
	for _, configsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("configsFirst=%t", configsFirst), func(t *testing.T) {
			idx := newNodeConfigIndex()
			worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "z-worker", Labels: map[string]string{"role": "worker"}}}
			infra := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a-infra", Labels: map[string]string{"role": "infra"}}}
			all := indexConfig("ns", "all", metav1.LabelSelector{})
			selected := indexConfig("ns", "selected", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "role", Operator: metav1.LabelSelectorOpIn, Values: []string{"worker"}}}})
			invalid := indexConfig("ns", "invalid", metav1.LabelSelector{MatchLabels: map[string]string{"role": "@"}})
			incoming := indexConfig("ns", "incoming", metav1.LabelSelector{})
			addNodes := func() { idx.upsertNode(worker); idx.upsertNode(infra) }
			addConfigs := func() { idx.upsertConfig(selected); idx.upsertConfig(all); idx.upsertConfig(invalid) }
			if configsFirst {
				addConfigs()
				addNodes()
			} else {
				addNodes()
				addConfigs()
			}
			both := map[string][]string{"a-infra": {"ns/all", "ns/incoming"}, "z-worker": {"ns/all", "ns/selected", "ns/incoming"}}
			assertIndexGroups(t, idx, incoming, both)
			// Resyncs and status-only updates must not duplicate memberships.
			addNodes()
			addConfigs()
			statusUpdate := worker.DeepCopy()
			statusUpdate.Status.Phase = corev1.NodeRunning
			idx.upsertNode(statusUpdate)
			assertIndexGroups(t, idx, incoming, both)
			// Content-only config updates must replace the stored object.
			content := all.DeepCopy()
			content.Spec.Raw.Config = "updated"
			idx.upsertConfig(content)
			groups, _ := idx.configsPerMatchingNode(incoming, labels.Everything())
			if groups[0].cfgs.Items[0].Spec.Raw.Config != "updated" {
				t.Fatal("content update was lost")
			}
			// A captured snapshot remains valid after subsequent event updates.
			moved := selected.DeepCopy()
			moved.Spec.NodeSelector.MatchExpressions[0].Values = []string{"infra"}
			idx.upsertConfig(moved)
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/all", "ns/selected", "ns/incoming"}, "z-worker": {"ns/all", "ns/incoming"}})
			if len(groups[1].cfgs.Items) != 3 {
				t.Fatal("event modified admission snapshot")
			}
			relabeled := worker.DeepCopy()
			relabeled.Labels["role"] = "infra"
			idx.upsertNode(relabeled)
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/all", "ns/selected", "ns/incoming"}, "z-worker": {"ns/all", "ns/selected", "ns/incoming"}})
			// Losing a required label removes memberships.
			noLabels := infra.DeepCopy()
			noLabels.Labels = nil
			idx.upsertNode(noLabels)
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/all", "ns/incoming"}, "z-worker": {"ns/all", "ns/selected", "ns/incoming"}})
			idx.deleteNode(toolscache.DeletedFinalStateUnknown{Key: worker.Name, Obj: relabeled})
			idx.deleteNode(worker)
			idx.deleteConfig(toolscache.DeletedFinalStateUnknown{Key: "ns/all", Obj: content})
			idx.deleteConfig(content)
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/incoming"}})
			idx.upsertNode(worker) // recreation evaluates current selectors
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/incoming"}, "z-worker": {"ns/incoming"}})
			// Valid -> invalid drops previous memberships, invalid -> valid restores them.
			bad := moved.DeepCopy()
			bad.Spec.NodeSelector = invalid.Spec.NodeSelector
			idx.upsertConfig(bad)
			idx.upsertNode(infra)
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/incoming"}, "z-worker": {"ns/incoming"}})
			idx.upsertConfig(moved)
			assertIndexGroups(t, idx, incoming, map[string][]string{"a-infra": {"ns/selected", "ns/incoming"}, "z-worker": {"ns/incoming"}})
		})
	}
}

func TestNodeConfigIndexIdentityAndWarnings(t *testing.T) {
	idx := newNodeConfigIndex()
	idx.upsertNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}})
	old := indexConfig("a", "same", metav1.LabelSelector{MatchLabels: map[string]string{"absent": "label"}})
	old.Spec.BGP.Routers = []v1beta1.Router{{Neighbors: []v1beta1.Neighbor{{ASN: 65001, Address: "192.0.2.1"}}}}
	other := old.DeepCopy()
	other.Namespace = "b"
	other.Spec.NodeSelector = metav1.LabelSelector{}
	idx.upsertConfig(other)
	idx.upsertConfig(old)
	incoming := old.DeepCopy()
	incoming.Spec.NodeSelector = metav1.LabelSelector{}
	incoming.Spec.BGP.Routers[0].Neighbors[0].EnableGracefulRestart = true
	assertIndexGroups(t, idx, incoming, map[string][]string{"node": {"b/same", "a/same"}})
	_, warnings := idx.configsPerMatchingNode(incoming, labels.Everything())
	if len(warnings) != 1 {
		t.Fatalf("expected one warning even when old selector did not match, got %v", warnings)
	}
	_, warnings = idx.configsPerMatchingNode(incoming, labels.Nothing())
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings without matching nodes: %v", warnings)
	}
	idx.deleteConfig(old)
	_, warnings = idx.configsPerMatchingNode(incoming, labels.Everything())
	if len(warnings) != 0 {
		t.Fatalf("another namespace generated warnings: %v", warnings)
	}
}

func TestNodeConfigIndexConcurrentSnapshots(t *testing.T) {
	idx := newNodeConfigIndex()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	cfg := indexConfig("ns", "existing", metav1.LabelSelector{})
	cfg.Spec.BGP.Routers = []v1beta1.Router{{ASN: 65001, Neighbors: []v1beta1.Neighbor{{ASN: 65002, Address: "192.0.2.1", PasswordSecret: v1beta1.SecretReference{Name: "secret", Namespace: "ns"}}}}}
	before := cfg.DeepCopy()
	incoming := indexConfig("ns", "incoming", metav1.LabelSelector{})
	idx.upsertNode(node)
	idx.upsertConfig(cfg)
	var wg sync.WaitGroup
	for worker := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				switch worker {
				case 0:
					idx.upsertConfig(cfg)
					idx.deleteConfig(cfg)
				case 1:
					idx.upsertNode(node)
					idx.deleteNode(node)
				case 2:
					groups, _ := idx.configsPerMatchingNode(incoming, labels.Everything())
					for _, group := range groups {
						if len(group.cfgs.Items) < 1 || len(group.cfgs.Items) > 2 || group.cfgs.Items[len(group.cfgs.Items)-1].Name != "incoming" {
							t.Error("inconsistent snapshot")
						}
						if err := controller.Validate(group.cfgs); err != nil {
							t.Errorf("snapshot validation failed: %v", err)
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	if diff := cmp.Diff(before, cfg); diff != "" {
		t.Fatalf("concurrent validation mutated informer object: %s", diff)
	}
}

type indexRegistration struct {
	toolscache.ResourceEventHandlerRegistration
	synced bool
}

func (r *indexRegistration) HasSynced() bool { return r.synced }

type indexInformer struct {
	cache.Informer
	registration *indexRegistration
	handler      toolscache.ResourceEventHandler
	err          error
	removed      bool
}

func (i *indexInformer) AddEventHandler(h toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	i.handler = h
	return i.registration, i.err
}
func (i *indexInformer) RemoveEventHandler(toolscache.ResourceEventHandlerRegistration) error {
	i.removed = true
	return nil
}
func (i *indexInformer) HasSynced() bool { return true }

func TestIndexHandlerSyncAndRegistrationErrors(t *testing.T) {
	nodes := &indexInformer{registration: &indexRegistration{}}
	configs := &indexInformer{registration: &indexRegistration{}}
	validator := &FRRConfigValidator{}
	if err := validator.registerIndexHandlers(nodes, configs); err != nil {
		t.Fatal(err)
	}
	for _, state := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		nodes.registration.synced = state[0]
		configs.registration.synced = state[1]
		if validator.cacheSynced() != (state[0] && state[1]) {
			t.Fatalf("incorrect handler readiness for %v", state)
		}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	cfg := indexConfig("ns", "existing", metav1.LabelSelector{})
	nodes.handler.OnAdd(node, true)
	configs.handler.OnAdd(cfg, true)
	incoming := indexConfig("ns", "incoming", metav1.LabelSelector{})
	assertIndexGroups(t, validator.index, incoming, map[string][]string{"node": {"ns/existing", "ns/incoming"}})
	changed := cfg.DeepCopy()
	changed.Spec.NodeSelector = metav1.LabelSelector{MatchLabels: map[string]string{"role": "worker"}}
	configs.handler.OnUpdate(cfg, changed)
	labeled := node.DeepCopy()
	labeled.Labels = map[string]string{"role": "worker"}
	nodes.handler.OnUpdate(node, labeled)
	assertIndexGroups(t, validator.index, incoming, map[string][]string{"node": {"ns/existing", "ns/incoming"}})
	configs.handler.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "ns/existing", Obj: changed})
	nodes.handler.OnDelete(node)
	assertIndexGroups(t, validator.index, incoming, map[string][]string{})
	sentinel := errors.New("registration failed")
	nodes.err = sentinel
	if err := validator.registerIndexHandlers(nodes, configs); !errors.Is(err, sentinel) {
		t.Fatalf("lost node registration error: %v", err)
	}
	nodes.err = nil
	configs.err = sentinel
	if err := validator.registerIndexHandlers(nodes, configs); !errors.Is(err, sentinel) || !nodes.removed {
		t.Fatalf("config error/cleanup missing: %v", err)
	}
}

// Compare event-maintained memberships with a fresh selector evaluation after
// arbitrary event interleavings, including selectors sensitive to absent labels.
func TestNodeConfigIndexMatchesFullScan(t *testing.T) {
	idx := newNodeConfigIndex()
	nodes := map[string]*corev1.Node{}
	configs := map[string]*v1beta1.FRRConfiguration{}
	random := rand.New(rand.NewSource(42))
	selectors := []metav1.LabelSelector{
		{},
		{MatchLabels: map[string]string{"role": "worker"}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "role", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"worker"}}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "role", Operator: metav1.LabelSelectorOpExists}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "role", Operator: metav1.LabelSelectorOpDoesNotExist}}},
		{MatchLabels: map[string]string{"role": "@"}},
	}
	incoming := indexConfig("ns", "incoming", metav1.LabelSelector{})
	for range 300 {
		name := fmt.Sprintf("object-%d", random.Intn(8))
		switch random.Intn(4) {
		case 0:
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
			switch random.Intn(3) {
			case 0:
				node.Labels = map[string]string{"role": "worker"}
			case 1:
				node.Labels = map[string]string{"role": "infra"}
			}
			nodes[name] = node
			idx.upsertNode(node)
		case 1:
			delete(nodes, name)
			idx.deleteNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		case 2:
			cfg := indexConfig("ns", name, selectors[random.Intn(len(selectors))])
			configs[name] = cfg
			idx.upsertConfig(cfg)
		case 3:
			delete(configs, name)
			idx.deleteConfig(indexConfig("ns", name, metav1.LabelSelector{}))
		}
		expected := map[string][]string{}
		for name, node := range nodes {
			keys := []string{}
			for _, cfg := range configs {
				selector, err := metav1.LabelSelectorAsSelector(&cfg.Spec.NodeSelector)
				if err == nil && selector.Matches(labels.Set(node.Labels)) {
					keys = append(keys, cfg.Namespace+"/"+cfg.Name)
				}
			}
			slices.Sort(keys)
			expected[name] = append(keys, "ns/incoming")
		}
		assertIndexGroups(t, idx, incoming, expected)
	}
}
